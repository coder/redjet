package redjet

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Error struct {
	raw string
}

func (e *Error) Error() string {
	return ""
}

// Result is the result of a command.
// Its methods are not safe for concurrent use.
type Result struct {
	mu sync.Mutex

	closeCh chan struct{}
	closed  int64

	err error

	conn   net.Conn
	rd     *bufio.Reader
	client *Client
}

func (r *Result) Error() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		return ""
	}
	return r.err.Error()
}

func readSimpleString(rd *bufio.Reader) ([]byte, error) {
	// Simple strings are typically small enough that we can don't care about the allocation.
	b, err := rd.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return b[:len(b)-2], nil
}

func readBulkString(c net.Conn, rd *bufio.Reader, w io.Writer) (int, error) {
	c.SetReadDeadline(time.Now().Add(time.Second * 5))
	b, err := rd.ReadBytes('\n')
	if err != nil {
		return 0, err
	}

	n, err := strconv.Atoi(string(b[:len(b)-2]))
	if err != nil {
		return 0, err
	}

	// Give about a second per byte.
	c.SetReadDeadline(time.Now().Add(
		time.Second*5 + (time.Duration(n) * time.Second),
	))

	if n == -1 {
		return 0, nil
	}
	var nn int64
	// CopyN should not allocate because rd is a bufio.Reader and implements
	// WriteTo.
	if nn, err = io.CopyN(w, rd, int64(n)); err != nil {
		return int(nn), err
	}
	// Discard CRLF
	if _, err := rd.Discard(2); err != nil {
		return int(nn), err
	}
	return int(nn), nil
}

func (r *Result) checkClosed() error {
	if atomic.LoadInt64(&r.closed) != 0 {
		return fmt.Errorf("result already closed")
	}
	return nil
}

var _ io.WriterTo = (*Result)(nil)

// WriteTo returns the result as a byte slice.
//
// It closes the result.
func (r *Result) WriteTo(w io.Writer) (int64, error) {
	if err := r.checkClosed(); err != nil {
		return 0, err
	}

	// Close must follow Unlock to avoid deadlock.
	defer r.Close()

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.err != nil {
		return 0, r.err
	}

	// The type byte should come fast since its so small. A timeout here implies
	// a protocol error.
	r.conn.SetDeadline(time.Now().Add(time.Second * 5))

	var typ byte
	typ, r.err = r.rd.ReadByte()
	if r.err != nil {
		r.err = fmt.Errorf("read type: %w", r.err)
		return 0, r.err
	}

	switch typ {
	case '+':
		// Simple string
		var s []byte
		s, r.err = readSimpleString(r.rd)
		if r.err != nil {
			return 0, r.err
		}
		var n int
		n, r.err = w.Write(s)
		return int64(n), r.err
	case '-':
		// Error
		var s []byte
		s, r.err = readSimpleString(r.rd)
		if r.err != nil {
			return 0, r.err
		}

		return 0, &Error{
			raw: string(s),
		}
	case '$':
		// Bulk string
		var n int
		n, r.err = readBulkString(r.conn, r.rd, w)
		return int64(n), r.err
	case ':':
		return 0, fmt.Errorf("unexpected integer response")
	case '*':
		return 0, fmt.Errorf("unexpected array response")
	default:
		return 0, fmt.Errorf("unknown type %q", typ)
	}
}

// Bytes returns the result as a byte slice.
//
// It closes the result.
func (r *Result) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	_, err := r.WriteTo(&buf)
	return buf.Bytes(), err
}

// Ok returns whether the result is "OK". Note that it may fail even if the
// command succeeded. For example, a successful GET will return a value.
func (r *Result) Ok() (bool, error) {
	got, err := r.Bytes()
	if err != nil {
		return false, err
	}
	return string(got) == "OK", nil
}

// Close releases all resources associated with the result.
// It is safe to call Close multiple times.
func (r *Result) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !atomic.CompareAndSwapInt64(&r.closed, 0, 1) {
		return nil
	}
	if r.closeCh != nil {
		close(r.closeCh)
	}
	if r.err == nil {
		r.client.putConn(r.conn)
	}
	bufReaderPool.Put(r.rd)
	return nil
}
