package redtest

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/redjet"
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

func StartRedisServer(t testing.TB, args ...string) (string, *redjet.Client) {
	socket := filepath.Join(t.TempDir(), "redis.sock")
	serverCmd := exec.Command(
		"redis-server", "--unixsocket", socket, "--loglevel", "debug",
		"--port", "0",
	)
	serverCmd.Args = append(serverCmd.Args, args...)
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

	var serverStarted int64

	time.AfterFunc(5*time.Second, func() {
		if atomic.LoadInt64(&serverStarted) == 0 {
			t.Errorf("redis-server failed to start")
			serverStdoutWr.Close()
		}
	})

	// Redis will print out the socket path when it's ready to server.
	sc := bufio.NewScanner(serverStdoutRd)
	for sc.Scan() {
		if !strings.Contains(sc.Text(), socket) && !strings.Contains(sc.Text(), "Ready to accept connections unix") {
			continue
		}
		atomic.StoreInt64(&serverStarted, 1)
		c := &redjet.Client{
			ConnectionPoolSize: 10,
			Dial: func(_ context.Context) (net.Conn, error) {
				return net.Dial("unix", socket)
			},
			IdleTimeout: 100 * time.Millisecond,
		}
		t.Cleanup(func() {
			err := c.Close()
			require.NoError(t, err)
		})
		return socket, c
	}
	t.Fatalf("failed to start redis-server, didn't find socket %q in stdout", socket)
	panic("unreachable")
}
