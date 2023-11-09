package redcache

import (
	"context"
	"testing"
	"time"

	"github.com/ammario/redjet/redtest"
	"github.com/stretchr/testify/require"
)

func TestCache(t *testing.T) {
	_, client := redtest.StartRedisServer(t)

	cache := &Cache[int]{
		Client: client,
		TTL:    1 * time.Second,
	}

	counter := 0
	ctx := context.Background()

	fn := func() (int, error) {
		counter++
		return counter, nil
	}

	for i := 0; i < 10; i++ {
		v, err := cache.Do(ctx, "foo", fn)
		if err != nil {
			t.Fatal(err)
		}
		require.Equal(t, 1, v)
	}

	time.Sleep(cache.TTL)

	v, err := cache.Do(ctx, "foo", fn)
	require.NoError(t, err)

	require.Equal(t, 2, v)
}
