// Package redcache provides a simple cache implementation using Redis as a backend.
package redcache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/redjet"
)

// Cache is a simple cache implementation using Redis as a backend.
type Cache[V any] struct {
	TTL    time.Duration
	Client *redjet.Client
	// Prefix is the prefix used for all keys in the cache.
	Prefix string
}

// Do executes fn and caches the result for key. If the value is already cached,
// it is returned immediately.
//
// Do uses JSON to marshal and unmarshal values. It may not perform well
// for large values.
func (c *Cache[V]) Do(
	ctx context.Context,
	key string,
	fn func() (V, error),
) (V, error) {
	fullKey := c.Prefix + key
	r := c.Client.Pipeline(ctx, nil, "GET", fullKey)
	defer r.Close()

	var v V

	got, err := r.Bytes()
	if err != nil {
		return v, err
	}

	if len(got) > 0 {
		err = json.Unmarshal(got, &v)
		if err != nil {
			return v, fmt.Errorf("unmarshal cached value: %w", err)
		}
		return v, nil
	}

	v, err = fn()
	if err != nil {
		return v, err
	}

	b, err := json.Marshal(v)
	if err != nil {
		return v, fmt.Errorf("marshal value: %w", err)
	}

	exp := c.TTL / time.Millisecond

	if exp == 0 {
		return v, fmt.Errorf("TTL is %v, but must be > %v", c.TTL, time.Millisecond)
	}

	r = c.Client.Pipeline(ctx, r, "SET", fullKey, b, "PX", int(exp))
	err = r.Ok()
	if err != nil {
		return v, fmt.Errorf("cache set: %w", err)
	}

	return v, nil
}
