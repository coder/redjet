package bench

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/redjet"
	"github.com/dustin/go-humanize"
	redigo "github.com/gomodule/redigo/redis"
	goredis "github.com/redis/go-redis/v9"
	"github.com/redis/rueidis"
	"github.com/stretchr/testify/require"
)

type testWriter struct {
	prefix string
	t      testing.TB
}

func (w *testWriter) Write(p []byte) (int, error) {
	sc := bufio.NewScanner(bytes.NewReader(p))
	for sc.Scan() {
		w.t.Logf("%s: %s", w.prefix, sc.Text())
	}
	return len(p), nil
}

func startRedisServer(t testing.TB) string {
	// We use a TCP port because it has much better performance.. for some
	// unknown reason.
	// Unfortunately, we need to bind to a port to easily use ruiedis.
	serverCmd := exec.Command(
		"redis-server", "--loglevel", "debug",
		"--bind", "127.0.0.1", "--port", "6380",
	)
	serverCmd.Dir = t.TempDir()

	serverStdoutRd, serverStdoutWr := io.Pipe()
	t.Cleanup(func() {
		serverStdoutWr.Close()
	})
	serverCmd.Stdout = io.MultiWriter(
		&testWriter{prefix: "server", t: t},
		serverStdoutWr,
	)
	serverCmd.Stderr = &testWriter{prefix: "server", t: t}

	err := serverCmd.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		serverCmd.Process.Kill()
	})

	const addr = "127.0.0.1:6380"
	// Redis will print out the socket path when it's ready to server.
	sc := bufio.NewScanner(serverStdoutRd)
	for sc.Scan() {
		if !strings.Contains(sc.Text(), "Ready to accept connections") {
			continue
		}
		return addr
	}
	t.Fatalf("failed to start redis-server")
	panic("unreachable")
}

var (
	payload1B = strings.Repeat("x", 1)
	payload1K = strings.Repeat("x", 1024)
	payload1M = strings.Repeat("x", 1024*1024)
)

type benchClient interface {
	get(b *testing.B, ctx context.Context, payload string)
}

type redjetClient struct {
	redjet.Client
}

func (c *redjetClient) get(b *testing.B, ctx context.Context, payload string) {
	err := c.Command(ctx, "SET", "foo", payload).Ok()
	require.NoError(b, err)

	var r *redjet.Result

	for i := 0; i < b.N; i++ {
		r = c.Pipeline(ctx, r, "GET", "foo")
	}

	for r.Next() {
		read, err := r.WriteTo(io.Discard)
		require.NoError(b, err)
		if read != int64(len(payload)) {
			b.Fatalf("read %d bytes, expected %d", read, len(payload))
		}
	}
}

type redigoClient struct {
	redigo.Conn
}

func (c *redigoClient) get(b *testing.B, ctx context.Context, payload string) {
	err := c.Send("SET", "foo", payload)
	require.NoError(b, err)

	for i := 0; i < b.N; i++ {
		c.Send("GET", "foo")
	}
	err = c.Flush()
	require.NoError(b, err)
	for i := 0; i < b.N; i++ {
		_, err = c.Receive()
		require.NoError(b, err)
	}
}

type goredisClient struct {
	*goredis.Client
}

func (c *goredisClient) get(b *testing.B, ctx context.Context, payload string) {
	err := c.Set(ctx, "foo", payload, 0).Err()
	require.NoError(b, err)

	pipe := c.Pipeline()

	var results []*goredis.StringCmd
	for i := 0; i < b.N; i++ {
		results = append(results, pipe.Get(ctx, "foo"))
	}

	cmds, err := pipe.Exec(ctx)
	require.NoError(b, err)

	require.Equal(b, b.N, len(cmds))

	for _, r := range results {
		s := r.Val()
		if len(s) != len(payload) {
			b.Fatalf("read %d bytes, expected %d", len(s), len(payload))
		}
	}
}

type rueidisClient struct {
	rueidis.Client
}

func (c *rueidisClient) get(b *testing.B, ctx context.Context, payload string) {
	var cmds rueidis.Commands
	cmds = append(cmds, c.B().Set().Key("foo").Value(payload).Build())

	for i := 0; i < b.N; i++ {
		cmds = append(cmds, c.B().Get().Key("foo").Build())
	}

	for _, resp := range c.DoMulti(ctx, cmds...) {
		require.NoError(b, resp.Error())
	}
}

var libFlag = flag.String("lib", "", "lib to benchmark")

func BenchmarkGet(b *testing.B) {
	addr := startRedisServer(b)

	flag.Parse()

	var client benchClient
	switch *libFlag {
	case "redjet":
		client = &redjetClient{
			redjet.Client{
				ConnectionPoolSize: 16,
				IdleTimeout:        10 * time.Second,
				Dial: func(_ context.Context) (net.Conn, error) {
					return net.Dial("tcp", addr)
				},
			},
		}
	case "redigo":
		conn, err := redigo.Dial("tcp", addr)
		require.NoError(b, err)
		client = &redigoClient{conn}
	case "go-redis":
		client = &goredisClient{goredis.NewClient(&goredis.Options{
			Network: "tcp",
			Addr:    addr,
		})}
	case "rueidis":
		c, err := rueidis.NewClient(rueidis.ClientOption{
			InitAddress: []string{"127.0.0.1:6380"},
		})
		require.NoError(b, err)
		client = &rueidisClient{c}
	case "":
		b.Fatalf("lib flag is required")
	default:
		b.Fatalf("unknown lib: %q", *libFlag)
	}

	ctx := context.Background()

	for _, payload := range []string{payload1B, payload1K, payload1M} {
		b.Run(humanize.Bytes(uint64(len(payload))), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			client.get(b, ctx, payload)
		})
	}
}
