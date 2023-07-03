package bench

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ammario/redjet"
	redigo "github.com/gomodule/redigo/redis"
	goredis "github.com/redis/go-redis/v9"
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
	socket := filepath.Join(t.TempDir(), "redis.sock")
	serverCmd := exec.Command(
		"redis-server", "--unixsocket", socket, "--loglevel", "debug",
		"--bind", "",
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

	// Redis will print out the socket path when it's ready to server.
	sc := bufio.NewScanner(serverStdoutRd)
	for sc.Scan() {
		if !strings.Contains(sc.Text(), socket) {
			continue
		}
		return socket
	}
	t.Fatalf("failed to start redis-server")
	panic("unreachable")
}

var big = strings.Repeat("x", 1024*1024)

func BenchmarkRedjet(b *testing.B) {
	socket := startRedisServer(b)
	client := redjet.Client{
		ConnectionPoolSize: 16,
		IdleTimeout:        10 * time.Second,
		Dial: func(_ context.Context) (net.Conn, error) {
			return net.Dial("unix", socket)
		},
	}

	ctx := context.Background()

	err := client.Command(ctx, "SET", "foo", big).Ok()
	require.NoError(b, err)

	var r *redjet.Result

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(len(big)))
	for i := 0; i < b.N; i++ {
		r = client.Pipeline(ctx, r, "GET", "foo")
	}

	for r.Next() {
		read, err := r.WriteTo(io.Discard)
		require.NoError(b, err)
		require.Equal(b, int64(len(big)), read)
	}
}

func BenchmarkRedigo(b *testing.B) {
	socket := startRedisServer(b)

	conn, err := redigo.Dial("unix", socket)
	require.NoError(b, err)

	err = conn.Send("SET", "foo", big)
	require.NoError(b, err)

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(len(big)))

	for i := 0; i < b.N; i++ {
		conn.Send("GET", "foo")
	}
	err = conn.Flush()
	require.NoError(b, err)
	for i := 0; i < b.N; i++ {
		_, err = conn.Receive()
		require.NoError(b, err)
	}
}

func BenchmarkGoRedis(b *testing.B) {
	socket := startRedisServer(b)
	rdb := goredis.NewClient(&goredis.Options{
		Network: "unix",
		Addr:    socket,
	})

	ctx := context.Background()
	err := rdb.Set(ctx, "foo", big, 0).Err()
	require.NoError(b, err)

	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(int64(len(big)))

	pipe := rdb.Pipeline()

	var results []*goredis.StringCmd
	for i := 0; i < b.N; i++ {
		results = append(results, pipe.Get(ctx, "foo"))
	}

	cmds, err := pipe.Exec(ctx)
	require.NoError(b, err)

	require.Equal(b, b.N, len(cmds))

	for _, r := range results {
		s := r.Val()

		require.Equal(b, len(big), len(s))
	}
}
