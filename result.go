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
	b, err := rd.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return b[:len(b)-2], nil
}

func readBulkString(c net.Conn, rd *bufio.Reader, w io.Writer) error {
	c.SetReadDeadline(time.Now().Add(time.Second * 5))
	b, err := rd.ReadBytes('\n')
	if err != nil {
		return err
	}

	n, err := strconv.Atoi(string(b[:len(b)-2]))
	if err != nil {
		return err
	}

	c.SetReadDeadline(time.Now().Add(
		time.Second*5 + (time.Duration(n) * time.Second),
	))

	if n == -1 {
		return nil
	}
	// CopyN should not allocate because rd is a bufio.Reader and implements
	// WriteTo.
	if _, err := io.CopyN(w, rd, int64(n)); err != nil {
		return err
	}
	// Discard CRLF
	if _, err := rd.Discard(2); err != nil {
		return err
	}
	return nil
}

func (r *Result) checkClosed() error {
	if atomic.LoadInt64(&r.closed) != 0 {
		return fmt.Errorf("result already closed")
	}
	return nil
}

// Bytes returns the result as a byte slice.
// If buf is nil, a new buffer is allocated.
//
// It closes the result.
func (r *Result) Bytes() ([]byte, error) {
	if err := r.checkClosed(); err != nil {
		return nil, err
	}

	// Close must follow Unlock to avoid deadlock.
	defer r.Close()

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.err != nil {
		return nil, r.err
	}

	// The type byte should come fast since its so small. A timeout here implies
	// a protocol error.
	r.conn.SetDeadline(time.Now().Add(time.Second * 5))

	var typ byte
	typ, r.err = r.rd.ReadByte()
	if r.err != nil {
		r.err = fmt.Errorf("read type: %w", r.err)
		return nil, r.err
	}

	switch typ {
	case '+':
		// Simple string
		var s []byte
		s, r.err = readSimpleString(r.rd)
		return s, r.err
	case '-':
		// Error
		var s []byte
		s, r.err = readSimpleString(r.rd)
		if r.err != nil {
			return nil, r.err
		}
		return nil, &Error{
			raw: string(s),
		}
	case '$':
		// Bulk string
		var buf bytes.Buffer
		r.err = readBulkString(r.conn, r.rd, &buf)
		return buf.Bytes(), r.err
	case ':':
		return nil, fmt.Errorf("unexpected integer response")
	case '*':
		return nil, fmt.Errorf("unexpected array response")
	default:
		return nil, fmt.Errorf("unknown type %q", typ)
	}
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
	close(r.closeCh)
	if r.err == nil {
		r.client.putConn(r.conn)
	}
	bufReaderPool.Put(r.rd)
	return nil
}
