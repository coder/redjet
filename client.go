package redjet

import (
	"bufio"
	"context"
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

func (c *Client) initPool() {
	c.poolMu.Lock()
	defer c.poolMu.Unlock()

	if c.pool == nil {
		c.pool = newConnPool(c.ConnectionPoolSize)
	}
}

func (c *Client) getConn(ctx context.Context) (net.Conn, error) {
	c.initPool()

	if conn, ok := c.pool.tryGet(ctx); ok {
		return conn, nil
	}

	return c.Dial(ctx)
}

func (c *Client) putConn(conn net.Conn) {
	c.initPool()
	// Clear any deadline.
	conn.SetDeadline(time.Time{})
	c.pool.put(conn, c.IdleTimeout)
}

const crlf = "\r\n"

var bufWriterPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewWriterSize(nil, 32*1024)
	},
}

var bufReaderPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewReaderSize(nil, 32*1024)
	},
}

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

// Command sends a command to the server and returns the result. The error
// is encoded into the result for ergonomics.
//
// The result must be closed or drained to avoid leaking resources.
func (c *Client) Command(ctx context.Context, cmd string, args ...any) (r *Result) {
	conn, err := c.getConn(ctx)
	if err != nil {
		return &Result{
			err: fmt.Errorf("get conn: %w", err),
		}
	}

	// The buffered writer reduces syscall overhead and gives us cleaner code.
	wr := bufWriterPool.Get().(*bufio.Writer)
	wr.Reset(conn)
	defer bufWriterPool.Put(wr)

	// We're instructing redis that we're sending an array of the command
	// and its arguments.
	wr.WriteString("*")
	wr.WriteString(strconv.Itoa(len(args) + 1))
	wr.WriteString(crlf)

	writeBulkString(wr, cmd)

	for _, arg := range args {
		switch arg := arg.(type) {
		case string:
			writeBulkString(wr, arg)
		case []byte:
			writeBulkBytes(wr, arg)
		default:
			r.err = fmt.Errorf("unsupported argument type %T", arg)
			conn.Close()
			return
		}
	}

	err = wr.Flush()
	if err != nil {
		conn.Close()
		return &Result{
			err: fmt.Errorf("write cmd: %w", err),
		}
	}

	rd := bufReaderPool.Get().(*bufio.Reader)
	rd.Reset(conn)

	success := &Result{
		conn:    conn,
		rd:      rd,
		client:  c,
		closeCh: make(chan struct{}),
	}
	go func() {
		select {
		case <-ctx.Done():
			success.Close()
		case <-success.closeCh:
		}
	}()
	return success
}
