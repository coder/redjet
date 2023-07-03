package redjet

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
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

func startRedisServer(t testing.TB) (string, *Client) {
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
		c := &Client{
			ConnectionPoolSize: 10,
			Dial: func(_ context.Context) (net.Conn, error) {
				return net.Dial("unix", socket)
			},
			IdleTimeout: 10 * time.Second,
		}
		t.Cleanup(func() {
			c.Close()
		})
		return socket, c
	}
	t.Fatalf("failed to start redis-server")
	panic("unreachable")
}

func TestClient_SetGet(t *testing.T) {
	_, client := startRedisServer(t)

	ctx := context.Background()

	gotOk, err := client.Command(ctx, "SET", "foo", "bar").Ok()
	require.NoError(t, err)
	require.True(t, gotOk)

	got, err := client.Command(ctx, "GET", "foo").Bytes()
	require.NoError(t, err)
	require.Equal(t, []byte("bar"), got)
}

func Benchmark_Get(b *testing.B) {
	_, client := startRedisServer(b)

	ctx := context.Background()

	payloadSizes := []int{1, 1024, 1024 * 1024}
	payloads := make([]string, len(payloadSizes))
	for i, payloadSize := range payloadSizes {
		payloads[i] = strings.Repeat("x", payloadSize)
	}

	b.ResetTimer()

	for i, payloadSize := range payloadSizes {
		b.Run("Size="+strconv.Itoa(payloadSize), func(b *testing.B) {
			payload := payloads[i]

			gotOk, err := client.Command(ctx, "SET", "foo", payload).Ok()
			require.NoError(b, err)
			require.True(b, gotOk)

			b.ResetTimer()
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for i := 0; i < b.N; i++ {
				n, err := client.Command(ctx, "GET", "foo").WriteTo(ioutil.Discard)
				require.NoError(b, err, "i=%d", i)
				if int(n) != len(payload) {
					b.Fatalf("unexpected written: %d", n)
				}
			}
		})
	}
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
