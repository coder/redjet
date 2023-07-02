package redjet

import (
	"context"
	"net"
	"time"
)

type Client struct {
	// MaxConnections limits the concurrency of the client.
	// If MaxConnections is 0, the client has unlimited concurrency.
	MaxConnections int

	// MinConnections is the minimum number of connections to keep open,
	// even if they are idle.
	MinConnections int

	// IdleTimeout is the amount of time after which an idle connection will
	// be closed.
	IdleTimeout time.Duration

	// Dial is the function used to create new connections.
	Dial func(ctx context.Context) (net.Conn, error)
}

// NewClient returns a new client that connects to addr with default settings.
func NewClient(addr string) *Client {
	return &Client{
		IdleTimeout: 5 * time.Minute,
		Dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
	}
}

func (c *Client) getConn(ctx context.Context) (net.Conn, error) {
	if c.MaxConnections == 0 {
		return c.Dial(ctx)
	}

	return nil, nil
}

func (c *Client) Command(ctx context.Context, cmd string, args ...string) (string, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	return Command(ctx, conn, cmd, args...)
}
