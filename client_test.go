package redjet

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func startRedisServer(t testing.TB, args ...string) (string, *Client) {
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
		if !strings.Contains(sc.Text(), socket) {
			continue
		}
		atomic.StoreInt64(&serverStarted, 1)
		c := &Client{
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
	t.Fatalf("failed to start redis-server")
	panic("unreachable")
}

func TestClient_SetGet(t *testing.T) {
	t.Parallel()

	_, client := startRedisServer(t)

	ctx := context.Background()

	err := client.Command(ctx, "SET", "foo", "bar").Ok()
	require.NoError(t, err)

	got, err := client.Command(ctx, "GET", "foo").Bytes()
	require.NoError(t, err)
	require.Equal(t, []byte("bar"), got)

	err = client.Command(ctx, "SET", "foo", []byte("bytebar")).Ok()
	require.NoError(t, err)

	got, err = client.Command(ctx, "GET", "foo").Bytes()
	require.NoError(t, err)
	require.Equal(t, []byte("bytebar"), got)
}

func TestClient_Race(t *testing.T) {
	t.Parallel()

	_, client := startRedisServer(t)

	ctx := context.Background()

	// Ensure connections are getted reused correctly.
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()

			key := strconv.Itoa(i)
			res := client.Command(ctx, "SET", key, "bar")
			if i%4 == 0 {
				err := res.Close()
				assert.NoError(t, err)
				return
			}

			err := res.Ok()
			assert.NoError(t, err)

			got, err := client.Command(ctx, "GET", key).Bytes()
			assert.NoError(t, err)
			assert.Equal(t, []byte("bar"), got)
		}()
	}

	wg.Wait()
}

func TestClient_IdleDrain(t *testing.T) {
	t.Parallel()

	_, client := startRedisServer(t)

	require.Equal(t, 0, client.freeConns())

	err := client.Command(context.Background(), "SET", "foo", "bar").Close()
	require.NoError(t, err)

	require.Equal(t, 1, client.freeConns())

	// After the idle timeout, the connection should be drained.
	require.Eventually(t, func() bool {
		return client.freeConns() == 0
	}, time.Second, 10*time.Millisecond)
}

func TestClient_ShortRead(t *testing.T) {
	t.Parallel()
	_, client := startRedisServer(t)
	// Test that the connection can be successfully re-used
	// even when pipeline is short-read.

	for i := 0; i < 100; i++ {
		var r *Result
		r = client.Pipeline(context.Background(), r, "SET", "foo", "bar")
		r = client.Pipeline(context.Background(), r, "GET", "foo")
		if 1%10 == 0 {
			err := r.Close()
			require.NoError(t, err)
			continue
		}
		err := r.Ok()
		require.NoError(t, err)

		got, err := r.Bytes()
		require.NoError(t, err)
		require.Equal(t, []byte("bar"), got)

		err = r.Close()
		require.NoError(t, err)
	}
}

func TestClient_LenReader(t *testing.T) {
	t.Parallel()

	_, client := startRedisServer(t)

	ctx := context.Background()

	v := strings.Repeat("x", 64)
	buf := bytes.NewBuffer(
		[]byte(v),
	)

	var _ LenReader = buf

	err := client.Command(ctx, "SET", "foo", buf).Ok()
	require.NoError(t, err)

	got, err := client.Command(ctx, "GET", "foo").Bytes()
	require.NoError(t, err)
	require.Equal(t, []byte(v), got)

	bigString := strings.Repeat("x", 1024)

	err = client.Command(ctx, "SET", "foo", NewLenReader(
		strings.NewReader(bigString),
		16,
	)).Ok()
	require.NoError(t, err)

	got, err = client.Command(ctx, "GET", "foo").Bytes()
	require.NoError(t, err)
	require.Equal(t, []byte(bigString)[:16], got)
}

func TestClient_BadCmd(t *testing.T) {
	t.Parallel()

	_, client := startRedisServer(t)

	ctx := context.Background()
	err := client.Command(ctx, "whatwhat").Ok()
	require.True(t, IsUnknownCommand(err))
	require.Error(t, err)
}

func TestClient_Integer(t *testing.T) {
	t.Parallel()

	_, client := startRedisServer(t)

	ctx := context.Background()
	err := client.Command(ctx, "SET", "foo", "123").Ok()
	require.NoError(t, err)

	got, err := client.Command(ctx, "GET", "foo").Int()
	require.NoError(t, err)
	require.EqualValues(t, 123, got)

	gotLen, err := client.Command(ctx, "STRLEN", "foo").Int()
	require.NoError(t, err)
	require.Equal(t, 3, gotLen)
}

func TestClient_AnyType(t *testing.T) {
	t.Parallel()

	_, client := startRedisServer(t)

	ctx := context.Background()
	err := client.Command(ctx, "SET", "foo", time.Hour).Ok()
	require.NoError(t, err)

	got, err := client.Command(ctx, "GET", "foo").String()
	require.NoError(t, err)
	require.EqualValues(t, "1h0m0s", got)
}

func TestClient_MGet(t *testing.T) {
	t.Parallel()

	_, client := startRedisServer(t)

	ctx := context.Background()
	err := client.Command(ctx, "MSET", "a", "antelope", "b", "bat", "c", "cat").Ok()
	require.NoError(t, err)

	cmd := client.Command(ctx, "MGET", "a", "b", "c")

	n, err := cmd.ArrayLength()
	require.NoError(t, err)
	require.Equal(t, 3, n)

	err = cmd.Close()
	require.NoError(t, err)

	got, err := client.Command(ctx, "MGET", "a", "b", "c").Strings()
	require.NoError(t, err, "read %+v", got)
	require.Equal(t, []string{"antelope", "bat", "cat"}, got)
}

func TestClient_Auth(t *testing.T) {
	t.Parallel()
	const password = "hunt12"

	t.Run("Fail", func(t *testing.T) {
		t.Parallel()

		_, client := startRedisServer(t, "--requirepass", password)
		ctx := context.Background()

		err := client.Command(ctx, "SET", "foo", "bar").Ok()
		require.Error(t, err)
		require.True(t, IsAuthError(err))
	})

	t.Run("Succeed", func(t *testing.T) {
		t.Parallel()

		_, client := startRedisServer(t, "--requirepass", password)
		ctx := context.Background()
		client.AuthPassword = password

		// It's imperative to test both SET and GET because the response
		// of SET matches the response of AUTH.
		err := client.Command(ctx, "SET", "foo", "flamingo").Ok()
		require.NoError(t, err)

		got, err := client.Command(ctx, "GET", "foo").Bytes()
		require.NoError(t, err)
		require.Equal(t, []byte("flamingo"), got)
	})
}

func TestClient_PubSub(t *testing.T) {
	t.Parallel()

	_, client := startRedisServer(t)

	ctx := context.Background()
	subCmd := client.Command(ctx, "SUBSCRIBE", "foo")
	defer subCmd.Close()

	msg, err := subCmd.NextSubMessage()
	require.NoError(t, err)

	require.Equal(t, &SubMessage{
		Channel: "foo",
		Type:    "subscribe",
		Payload: "1",
	}, msg)

	n, err := client.Command(ctx, "PUBLISH", "foo", "bar").Int()
	require.NoError(t, err)
	require.Equal(t, 1, n)

	msg, err = subCmd.NextSubMessage()
	require.NoError(t, err)

	require.Equal(t, &SubMessage{
		Channel: "foo",
		Type:    "message",
		Payload: "bar",
	}, msg)
}

func TestClient_ConnReuse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	// This test is not parallel because it is sensitive to timing.

	socket, client := startRedisServer(t)

	var connsMade int64
	client.Dial = func(_ context.Context) (net.Conn, error) {
		atomic.AddInt64(&connsMade, 1)
		return net.Dial("unix", socket)
	}

	// Test that connections aren't created unnecessarily.

	start := time.Now()
	for i := 0; i < 16; i++ {
		time.Sleep(client.IdleTimeout / 4)

		err := client.Command(context.Background(), "SET", "foo", "bar").Ok()
		require.NoError(t, err)

		t.Logf("i=%d, sinceStart=%v", i, time.Since(start))
		require.Equal(t, int64(1+i), atomic.LoadInt64(&client.pool.returns))
		require.Equal(t, int64(0), atomic.LoadInt64(&client.pool.fullPoolCloses))
		require.Equal(t, int64(1), atomic.LoadInt64(&connsMade))
		require.Equal(t, 1, client.freeConns())
	}

	time.Sleep(client.IdleTimeout * 4)

	require.Equal(t, int64(1), atomic.LoadInt64(&connsMade))
	require.Equal(t, 0, client.freeConns())

	// The clean cycle should've ran more than once by now.
	require.Greater(t,
		atomic.LoadInt64(&client.pool.cleanCycles), int64(1),
	)
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

			err := client.Command(ctx, "SET", "foo", payload).Ok()
			require.NoError(b, err)

			b.ResetTimer()
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))

			const (
				batchSize = 100
			)
			var r *Result
			get := func() {
				for r.Next() {
					n, err := r.WriteTo(io.Discard)
					require.NoError(b, err)
					require.EqualValues(b, len(payload), n)
				}
			}
			for i := 0; i < b.N; i++ {
				r = client.Pipeline(ctx, r, "GET", "foo")
				if (i+1)%batchSize == 0 {
					get()
				}
			}
			get()
			err = r.Close()
			require.NoError(b, err)
			b.StopTimer()
			time.Sleep(100 * time.Millisecond)
		})
	}
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
