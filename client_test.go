package redjet_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ammario/redjet"
	"github.com/ammario/redjet/redtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestClient_SetGet(t *testing.T) {
	t.Parallel()

	_, client := redtest.StartRedisServer(t)

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

func TestClient_NotFound(t *testing.T) {
	t.Parallel()

	_, client := redtest.StartRedisServer(t)

	ctx := context.Background()

	got, err := client.Command(ctx, "GET", "brah").String()
	require.NoError(t, err)
	require.Equal(t, "", got)
}

func TestClient_List(t *testing.T) {
	t.Parallel()

	_, client := redtest.StartRedisServer(t)

	ctx := context.Background()

	arr, err := client.Command(ctx, "LRANGE", "foo", "-1", -1).Strings()
	require.NoError(t, err)
	require.Empty(t, arr)

	n, err := client.Command(ctx, "RPUSH", "foo", "bar").Int()
	require.NoError(t, err)
	require.Equal(t, 1, n)

	arr, err = client.Command(ctx, "LRANGE", "foo", "-1", -1).Strings()
	require.NoError(t, err)
	require.Equal(t, []string{"bar"}, arr)
}

func TestClient_Race(t *testing.T) {
	t.Parallel()

	_, client := redtest.StartRedisServer(t)

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

	_, client := redtest.StartRedisServer(t)

	require.Equal(t, 0, client.PoolStats().FreeConns)

	err := client.Command(context.Background(), "SET", "foo", "bar").Close()
	require.NoError(t, err)

	require.Equal(t, 1, client.PoolStats().FreeConns)

	// After the idle timeout, the connection should be drained.
	require.Eventually(t, func() bool {
		return client.PoolStats().FreeConns == 0
	}, time.Second, 10*time.Millisecond)
}

func TestClient_ShortRead(t *testing.T) {
	t.Parallel()
	_, client := redtest.StartRedisServer(t)
	// Test that the connection can be successfully re-used
	// even when pipeline is short-read.

	for i := 0; i < 100; i++ {
		var r *redjet.Pipeline
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

	_, client := redtest.StartRedisServer(t)

	ctx := context.Background()

	v := strings.Repeat("x", 64)
	buf := bytes.NewBuffer(
		[]byte(v),
	)

	var _ redjet.LenReader = buf

	err := client.Command(ctx, "SET", "foo", buf).Ok()
	require.NoError(t, err)

	got, err := client.Command(ctx, "GET", "foo").Bytes()
	require.NoError(t, err)
	require.Equal(t, []byte(v), got)

	bigString := strings.Repeat("x", 1024)

	err = client.Command(ctx, "SET", "foo", redjet.NewLenReader(
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

	_, client := redtest.StartRedisServer(t)

	ctx := context.Background()
	err := client.Command(ctx, "whatwhat").Ok()
	require.True(t, redjet.IsUnknownCommand(err))
	require.Error(t, err)
}

func TestClient_Integer(t *testing.T) {
	t.Parallel()

	_, client := redtest.StartRedisServer(t)

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

func TestClient_Stringer(t *testing.T) {
	t.Parallel()

	_, client := redtest.StartRedisServer(t)

	ctx := context.Background()
	err := client.Command(ctx, "SET", "foo", time.Hour).Ok()
	require.NoError(t, err)

	got, err := client.Command(ctx, "GET", "foo").String()
	require.NoError(t, err)
	require.EqualValues(t, "1h0m0s", got)
}

func TestClient_JSON(t *testing.T) {
	t.Parallel()

	_, client := redtest.StartRedisServer(t)

	var v struct {
		Foo string
		Bar int
	}

	v.Foo = "bar"
	v.Bar = 123

	ctx := context.Background()
	err := client.Command(ctx, "SET", "foo", v).Ok()
	require.NoError(t, err)

	resp := make(map[string]interface{})
	err = client.Command(ctx, "GET", "foo").JSON(&resp)
	require.NoError(t, err)
	require.Equal(t, "bar", resp["Foo"])
	require.Equal(t, float64(123), resp["Bar"])
}

func TestClient_MGet(t *testing.T) {
	t.Parallel()

	_, client := redtest.StartRedisServer(t)

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

		_, client := redtest.StartRedisServer(t, "--requirepass", password)
		ctx := context.Background()

		err := client.Command(ctx, "SET", "foo", "bar").Ok()
		require.Error(t, err)
		require.True(t, redjet.IsAuthError(err))
	})

	t.Run("Succeed", func(t *testing.T) {
		t.Parallel()

		_, client := redtest.StartRedisServer(t, "--requirepass", password)
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

	_, client := redtest.StartRedisServer(t)

	ctx := context.Background()
	subCmd := client.Command(ctx, "SUBSCRIBE", "foo")
	defer subCmd.Close()

	msg, err := subCmd.NextSubMessage()
	require.NoError(t, err)

	require.Equal(t, &redjet.SubMessage{
		Channel: "foo",
		Type:    "subscribe",
		Payload: "1",
	}, msg)

	n, err := client.Command(ctx, "PUBLISH", "foo", "bar").Int()
	require.NoError(t, err)
	require.Equal(t, 1, n)

	msg, err = subCmd.NextSubMessage()
	require.NoError(t, err)

	require.Equal(t, &redjet.SubMessage{
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

	socket, client := redtest.StartRedisServer(t)

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
		stats := client.PoolStats()
		require.Equal(t, int64(1+i), stats.Returns)
		require.Equal(t, int64(0), stats.FullPoolCloses)
		require.Equal(t, int64(1), connsMade)
		require.Equal(t, 1, stats.FreeConns)
	}

	time.Sleep(client.IdleTimeout * 4)

	stats := client.PoolStats()
	require.Equal(t, int64(1), atomic.LoadInt64(&connsMade))
	require.Equal(t, 0, stats.FreeConns)

	// The clean cycle should've ran more than once by now.
	require.Greater(t,
		stats.CleanCycles, int64(1),
	)
}

func Benchmark_Get(b *testing.B) {
	_, client := redtest.StartRedisServer(b)

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
			var r *redjet.Pipeline
			// We avoid assert/require in the hot path since it meaningfully
			// affects the benchmark.
			get := func() {
				for r.Next() {
					n, err := r.WriteTo(io.Discard)
					require.NoError(b, err)
					if n != int64(len(payload)) {
						b.Fatalf("expected %d bytes, got %d", len(payload), n)
					}
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
		})
	}
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
