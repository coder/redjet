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
