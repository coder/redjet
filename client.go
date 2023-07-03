package redjet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

type Client struct {
	// ConnectionPoolSize limits the size of the connection pool. If 0, connections
	// are not pooled.
	ConnectionPoolSize int

	// IdleTimeout is the amount of time after which an idle connection will
	// be closed.
	IdleTimeout time.Duration

	// Dial is the function used to create new connections.
	Dial func(ctx context.Context) (net.Conn, error)

	poolMu sync.Mutex
	pool   *connPool
}

// NewClient returns a new client that connects to addr with default settings.
func NewClient(addr string) *Client {
	c := &Client{
		ConnectionPoolSize: 8,
		IdleTimeout:        5 * time.Minute,
		Dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
	}
	return c
}

func (c *Client) freeConns() int {
	if c.pool == nil {
		return 0
	}
	return len(c.pool.free)
}

func (c *Client) initPool() {
	c.poolMu.Lock()
	defer c.poolMu.Unlock()

	if c.pool == nil {
		c.pool = newConnPool(c.ConnectionPoolSize, c.IdleTimeout)
	}
}

func (c *Client) getConn(ctx context.Context) (*conn, error) {
	c.initPool()

	if conn, ok := c.pool.tryGet(ctx); ok {
		return conn, nil
	}

	nc, err := c.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	return &conn{
		Conn:     nc,
		lastUsed: time.Now(),
		wr:       bufio.NewWriter(nc),
		rd:       bufio.NewReader(nc),
	}, nil
}

func (c *Client) putConn(conn *conn) {
	if conn == nil {
		panic("cannot put nil conn")
	}
	c.initPool()
	// Clear any deadline.
	conn.SetDeadline(time.Time{})
	c.pool.put(conn)
}

const crlf = "\r\n"

func writeBulkString(w *bufio.Writer, s string) {
	w.WriteString("$")
	w.WriteString(strconv.Itoa(len(s)))
	w.WriteString(crlf)
	w.WriteString(s)
	w.WriteString(crlf)
}

func writeBulkBytes(w *bufio.Writer, b []byte) {
	w.WriteString("$")
	w.WriteString(strconv.Itoa(len(b)))
	w.WriteString(crlf)
	w.Write(b)
	w.WriteString(crlf)
}

// Pipeline sends a command to the server and returns the promise of a result.
// r may be nil, as in the case of the first command in a pipeline. Each successive
// call to Pipeline should re-use the last returned Result.
//
// It is safe to keep a pipeline running for a long time, with many send and
// receive cycles.
func (c *Client) Pipeline(ctx context.Context, r *Result, cmd string, args ...any) *Result {
	var (
		conn *conn
		err  error
	)
	if r == nil {
		conn, err = c.getConn(ctx)
		if err != nil {
			return &Result{
				err: fmt.Errorf("get conn: %w", err),
			}
		}
	} else {
		conn = r.conn
	}

	// We're instructing redis that we're sending an array of the command
	// and its arguments.
	conn.wr.WriteString("*")
	conn.wr.WriteString(strconv.Itoa(len(args) + 1))
	conn.wr.WriteString(crlf)

	writeBulkString(conn.wr, cmd)

	for _, arg := range args {
		switch arg := arg.(type) {
		case string:
			writeBulkString(conn.wr, arg)
		case []byte:
			writeBulkBytes(conn.wr, arg)
		default:
			conn.Close()
			return &Result{
				err: fmt.Errorf("unsupported argument type %T", arg),
			}
		}
	}

	if r == nil {
		r = &Result{
			conn:    conn,
			client:  c,
			closeCh: make(chan struct{}),
			pipeline: pipeline{
				at:  0,
				end: 1,
			},
			CloseOnRead: false,
		}
		go func() {
			select {
			case <-ctx.Done():
				r.Close()
			case <-r.closeCh:
			}
		}()
	} else {
		r.pipeline.end++
	}
	return r
}

// Command sends a command to the server and returns the result. The error
// is encoded into the result for ergonomics.
//
// The result must be closed or drained to avoid leaking resources.
func (c *Client) Command(ctx context.Context, cmd string, args ...any) *Result {
	r := c.Pipeline(ctx, nil, cmd, args...)
	r.CloseOnRead = true
	return r
}

func (c *Client) Close() error {
	c.poolMu.Lock()
	defer c.poolMu.Unlock()

	var merr error
	if c.pool != nil {
		close(c.pool.cancelClean)
		<-c.pool.cleanExited
		// The cleaner may read free until it exits.
		close(c.pool.free)
		for conn := range c.pool.free {
			err := conn.Close()
			merr = errors.Join(merr, err)
		}
		c.pool.cleanTicker.Stop()
		c.pool = nil
	}
	return merr
}
