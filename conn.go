package redjet

import (
	"bufio"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type conn struct {
	net.Conn
	wr *bufio.Writer
	rd *bufio.Reader
	// miscBuf is used when bufio abstractions get in the way.
	miscBuf  []byte
	lastUsed time.Time
}

type connPool struct {
	free chan *conn

	cancelClean chan struct{}
	cleanExited chan struct{}
	cleanTicker *time.Ticker

	cleanMu sync.Mutex

	// These metrics are only used for testing.
	cleanCycles    int64
	returns        int64
	fullPoolCloses int64

	idleTimeout time.Duration
}

func newConnPool(size int, idleTimeout time.Duration) *connPool {
	p := &connPool{
		free: make(chan *conn, size),
		// 2 is chosen arbitrarily.
		cleanTicker: time.NewTicker(idleTimeout * 2),

		cancelClean: make(chan struct{}),
		cleanExited: make(chan struct{}),
		idleTimeout: idleTimeout,
	}
	go p.cleanLoop()
	return p
}

func (p *connPool) clean() {
	p.cleanMu.Lock()
	defer p.cleanMu.Unlock()

	atomic.AddInt64(&p.cleanCycles, 1)

	var (
		ln     = len(p.free)
		closed int
	)
	// While the cleanMu is held, no getConn or putConn operations can happen.
	// Thus, none of these operations can block.
	for i := 0; i < ln; i++ {
		c, ok := <-p.free
		if !ok {
			panic("pool closed improperly")
		}
		if time.Since(c.lastUsed) > p.idleTimeout {
			c.Close()
			closed++
			continue
		}

		p.free <- c
	}

	if len(p.free) != ln-closed {
		panic("pool size changed during clean")
	}
}

func (p *connPool) cleanLoop() {
	defer close(p.cleanExited)
	// We use a centralized routine for cleaning instead of AfterFunc on each
	// connection because the latter creates more garbage, even though it scales
	// logarithmically as opposed to linearly.
	for {
		select {
		case <-p.cancelClean:
			return
		case <-p.cleanTicker.C:
			p.clean()
		}
	}
}

// tryGet tries to get a connection from the pool. If there are no free
// connections, it returns false.
func (p *connPool) tryGet() (*conn, bool) {
	p.cleanMu.Lock()
	defer p.cleanMu.Unlock()

	select {
	case c, ok := <-p.free:
		if !ok {
			return nil, false
		}
		return c, true
	default:
		return nil, false
	}
}

// put returns a connection to the pool.
// If the pool is full, the connection is closed.
func (p *connPool) put(c *conn) {
	p.cleanMu.Lock()
	defer p.cleanMu.Unlock()

	c.lastUsed = time.Now()

	atomic.AddInt64(&p.returns, 1)

	select {
	case p.free <- c:
	default:
		atomic.AddInt64(&p.fullPoolCloses, 1)
		// Pool is full, just close the connection.
		c.Close()
	}
}
