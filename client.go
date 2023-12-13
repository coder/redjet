package redjet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	// AuthUsername is the username used for authentication.
	//
	// If set, AuthPassword must also be set. If not using Redis ACLs, just
	// set AuthPassword.
	//
	// See more: https://redis.io/commands/auth/
	AuthUsername string
	// AuthPassword is the password used for authentication.
	// Authentication must be set before any other commands are sent, and
	// must not change during the lifetime of the client.
	//
	// See more: https://redis.io/commands/auth/
	AuthPassword string

	poolMu sync.Mutex
	pool   *connPool
}

// New returns a new client that connects to addr with default settings.
func New(addr string) *Client {
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
		c.pool = newConnPool(c.ConnectionPoolSize, c.IdleTimeout)
	}
}

type PoolStats struct {
	FreeConns      int
	Returns        int64
	FullPoolCloses int64
	CleanCycles    int64
}

// PoolStats returns statistics about the connection pool.
func (c *Client) PoolStats() PoolStats {
	if c.pool == nil {
		return PoolStats{}
	}
	return PoolStats{
		FreeConns:      len(c.pool.free),
		Returns:        atomic.LoadInt64(&c.pool.returns),
		FullPoolCloses: atomic.LoadInt64(&c.pool.fullPoolCloses),
		CleanCycles:    atomic.LoadInt64(&c.pool.cleanCycles),
	}
}

// getConn gets a new conn, wrapped in a Pipeline. The conn is already authenticated.
func (c *Client) getConn(ctx context.Context) (*Pipeline, error) {
	c.initPool()

	if conn, ok := c.pool.tryGet(ctx); ok {
		return c.newResult(conn), nil
	}

	nc, err := c.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	conn := &conn{
		Conn:     nc,
		lastUsed: time.Now(),
		wr:       bufio.NewWriterSize(nc, 32*1024),
		rd:       bufio.NewReaderSize(nc, 32*1024),
		miscBuf:  make([]byte, 32*1024),
	}

	r := c.newResult(conn)

	if c.AuthUsername == "" && c.AuthPassword == "" {
		return r, nil
	}

	if c.AuthUsername != "" && c.AuthPassword == "" {
		nc.Close()
		return nil, errors.New("auth username set but password not set")
	}

	if c.AuthUsername != "" {
		r = c.Pipeline(ctx, r, "AUTH", c.AuthUsername, c.AuthPassword)
	} else {
		r = c.Pipeline(ctx, r, "AUTH", c.AuthPassword)
	}

	err = r.Ok()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("auth: %w", err)
	}

	return r, nil
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

var crlf = []byte("\r\n")

func writeBulkString(w *bufio.Writer, s string) {
	w.WriteString("$")
	w.WriteString(strconv.Itoa(len(s)))
	w.Write(crlf)
	w.WriteString(s)
	w.Write(crlf)
}

func writeBulkBytes(w *bufio.Writer, b []byte) {
	w.WriteString("$")
	w.WriteString(strconv.Itoa(len(b)))
	w.Write(crlf)
	w.Write(b)
	w.Write(crlf)
}

func writeBulkReader(w *bufio.Writer, rd LenReader) {
	w.WriteString("$")
	w.WriteString(strconv.Itoa(rd.Len()))
	w.Write(crlf)
	io.CopyN(w, rd, int64(rd.Len()))
	w.Write(crlf)
}

// LenReader is an io.Reader that also knows its length.
// A new one may be created with NewLenReader.
type LenReader interface {
	Len() int
	io.Reader
}

type lenReader struct {
	io.Reader
	size int
}

func (r *lenReader) Len() int {
	return r.size
}

func NewLenReader(r io.Reader, size int) LenReader {
	return &lenReader{
		Reader: r,
		size:   size,
	}
}

func (c *Client) newResult(conn *conn) *Pipeline {
	return &Pipeline{
		closeCh: make(chan struct{}),
		conn:    conn,
		client:  c,
	}
}

// Pipeline sends a command to the server and returns the promise of a result.
// r may be nil, as in the case of the first command in a pipeline. Each successive
// call to Pipeline should re-use the last returned Pipeline.
//
// Known arg types are strings, []byte, LenReader, and fmt.Stringer. All other types
// will be converted to JSON.
//
// It is safe to keep a pipeline running for a long time, with many send and
// receive cycles.
//
// Example:
//
//	p := client.Pipeline(ctx, nil, "SET", "foo", "bar")
//	defer p.Close()
//
//	p = client.Pipeline(ctx, r, "GET", "foo")
//	// Read the result of SET first.
//	err := p.Ok()
//	if err != nil {
//		// handle error
//	}
//
//	got, err := p.Bytes()
//	if err != nil {
//		// handle error
//	}
//	fmt.Println(string(got))
func (c *Client) Pipeline(ctx context.Context, r *Pipeline, cmd string, args ...any) *Pipeline {
	var err error
	if r == nil {
		r, err = c.getConn(ctx)
		if err != nil {
			return &Pipeline{
				err: fmt.Errorf("get conn: %w", err),
			}
		}

		// We must take great care that Close is eventually called on the result.
		go func() {
			select {
			case <-ctx.Done():
				r.Close()
			case <-r.closeCh:
			}
		}()
	}

	cmd = strings.ToUpper(cmd)
	// Redis already gives a nice error if we send a non-subscribe command
	// while in subscribe mode.
	if isSubscribeCmd(cmd) {
		r.subscribeMode = true
	}

	// We're instructing redis that we're sending an array of the command
	// and its arguments.
	r.conn.wr.WriteByte('*')
	r.conn.wr.WriteString(strconv.Itoa(len(args) + 1))
	r.conn.wr.Write(crlf)

	writeBulkString(r.conn.wr, cmd)

	for _, arg := range args {
		switch arg := arg.(type) {
		case string:
			writeBulkString(r.conn.wr, arg)
		case []byte:
			writeBulkBytes(r.conn.wr, arg)
		case LenReader:
			writeBulkReader(r.conn.wr, arg)
		case fmt.Stringer:
			writeBulkString(r.conn.wr, arg.String())
		default:
			v, err := json.Marshal(arg)
			if err != nil {
				// It's relatively rare to get an error here.
				panic(fmt.Sprintf("failed to marshal %T: %v", arg, err))
			}
			writeBulkBytes(r.conn.wr, v)
		}
	}

	r.pipeline.end++
	return r
}

// Command sends a command to the server and returns the result. The error
// is encoded into the result for ergonomics.
//
// See Pipeline for more information on argument types.
//
// The caller should call Close on the result when finished with it.
func (c *Client) Command(ctx context.Context, cmd string, args ...any) *Pipeline {
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
