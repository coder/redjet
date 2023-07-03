package redjet

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync/atomic"
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
	closed int64

	err error

	conn   net.Conn
	rd     *bufio.Reader
	client *Client
}

func (r *Result) Error() string {
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

func readBulkString(rd *bufio.Reader, w io.Writer) error {
	b, err := rd.ReadBytes('\n')
	if err != nil {
		return err
	}

	n, err := strconv.Atoi(string(b[:len(b)-2]))
	if err != nil {
		return err
	}

	if n == -1 {
		return nil
	}
	// CopyN should not allocate since rd is a bufio.Reader and implements
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
//
// It closes the result.
func (r *Result) Bytes() ([]byte, error) {
	if err := r.checkClosed(); err != nil {
		return nil, err
	}
	defer r.Close()

	if r.err != nil {
		return nil, r.err
	}

	var typ byte
	typ, r.err = r.rd.ReadByte()
	if r.err != nil {
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
		var buf bytes.Buffer
		// Bulk string
		r.err = readBulkString(r.rd, &buf)
		return buf.Bytes(), r.err
	case ':':
		return nil, fmt.Errorf("unexpected integer response")
	case '*':
		return nil, fmt.Errorf("unexpected array response")
	default:
		return nil, fmt.Errorf("unknown type %q", typ)
	}
}

// Close releases all resources associated with the result.
// It is safe to call Close multiple times.
func (r *Result) Close() error {
	if !atomic.CompareAndSwapInt64(&r.closed, 0, 1) {
		return nil
	}

	if r.err == nil {
		r.client.putConn(r.conn)
	}
	bufReaderPool.Put(r.rd)
	return nil
}
