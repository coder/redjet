package redjet

import (
	"context"
	"net"
	"time"
)

type conn struct {
	net.Conn

	idleCloseTimer *time.Timer
}

type connPool struct {
	free chan *conn
}

func newConnPool(size int) *connPool {
	return &connPool{
		free: make(chan *conn, size),
	}
}

// tryGet tries to get a connection from the pool. If there are no free
// connections, it returns false.
func (p *connPool) tryGet(ctx context.Context) (*conn, bool) {
	select {
	case c := <-p.free:
		if !c.idleCloseTimer.Stop() {
			// Timer already fired, so the connection was closed.
			// Try again.
			return p.tryGet(ctx)
		}
		return c, true
	default:
		return nil, false
	}
}

// put returns a connection to the pool.
// If the pool is full, the connection is closed.
func (p *connPool) put(nc net.Conn, idleTimeout time.Duration) {
	c := &conn{
		Conn: nc,
	}
	if idleTimeout <= 0 {
		panic("idleTimeout must be > 0")
	}
	c.idleCloseTimer = time.AfterFunc(idleTimeout, func() {
		c.Close()
	})

	select {
	case p.free <- c:
	default:
		// Pool is full, just close the connection.
		c.Close()
	}
}
