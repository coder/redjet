package redjet

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Error represents an error returned by the Redis server, as opposed to a
// a protocol or network error.
type Error struct {
	raw string
}

func (e *Error) Error() string {
	return e.raw
}

// IsUnknownCommand returns whether err is an "unknown command" error.
func IsUnknownCommand(err error) bool {
	var e *Error
	return errors.As(err, &e) && strings.HasPrefix(e.raw, "ERR unknown command")
}

func IsAuthError(err error) bool {
	var e *Error
	return errors.As(err, &e) && strings.HasPrefix(e.raw, "NOAUTH")
}

// pipeline contains state of a Redis pipeline.
type pipeline struct {
	at  int
	end int
}

// Result is the result of a command.
//
// Its methods are not safe for concurrent use.
type Result struct {
	// CloseOnRead determines whether the result is closed after the first read.
	//
	// It is set to True when the result is returned from Command, and
	// False when it is returned from Pipeline.
	//
	// It is ignored if in the middle of reading an array, or if the result
	// is of a subscribe command.
	CloseOnRead bool

	// subscribeMode is set to true when the result is returned from Subscribe.
	// In this case, the connection cannot be reused.
	subscribeMode bool

	mu sync.Mutex

	closeCh chan struct{}
	closed  int64

	err error

	conn   *conn
	client *Client

	pipeline pipeline

	// arrayStack tracks the depth of the array processing. E.g. if we're one
	// level deep with 3 elements remaining, arrayStack will be [3].
	arrayStack []int
}

func (r *Result) Error() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err == nil {
		return ""
	}
	return r.err.Error()
}

// readUntilNewline reads until a newline, returning the bytes without the newline.
// the returned bytes are built on top of buf.
func readUntilNewline(rd *bufio.Reader, buf []byte) ([]byte, error) {
	buf = buf[:0]
	for {
		b, err := rd.ReadByte()
		if err != nil {
			return nil, err
		}
		buf = append(buf, b)
		if b == '\n' {
			break
		}
	}
	return buf[:len(buf)-2], nil
}

// grower represents memory-buffer types that can grow to a given size.
type grower interface {
	Grow(int)
}

var (
	_ grower = (*bytes.Buffer)(nil)
	_ grower = (*strings.Builder)(nil)
)

func readBulkString(w io.Writer, c net.Conn, rd *bufio.Reader, copyBuf []byte) (int, error) {
	newlineBuf, err := readUntilNewline(rd, copyBuf)
	if err != nil {
		return 0, err
	}

	stringSize, err := strconv.Atoi(string(newlineBuf))
	if err != nil {
		return 0, err
	}

	// n == -1 signals a nil value.
	if stringSize <= 0 {
		return 0, nil
	}

	if g, ok := w.(grower); ok {
		g.Grow(stringSize)
	}

	// io.CopyN will allocate a buffer of size N when N is small, so we
	// replace the behavior here.

	var nn int64
	if stringSize > len(copyBuf) {
		nn, err = io.CopyBuffer(w, &io.LimitedReader{
			R: rd,
			N: int64(stringSize),
		}, copyBuf)
		if err != nil {
			return int(nn), err
		}
	} else {
		// Fast-path for small strings.
		// This avoids allocating a LimitReader.
		buf := copyBuf[:stringSize]
		n, err := io.ReadFull(rd, buf)
		if err != nil {
			return n, err
		}

		n, err = w.Write(buf)
		if err != nil {
			return n, err
		}
		nn = int64(n)
	}

	// Discard CRLF
	if _, err := rd.Discard(2); err != nil {
		return int(nn), err
	}
	return int(nn), nil
}

var errClosed = fmt.Errorf("result closed")

func (r *Result) checkClosed() error {
	if atomic.LoadInt64(&r.closed) != 0 {
		return errClosed
	}
	return nil
}

var _ io.WriterTo = (*Result)(nil)

// WriteTo writes the result to w.
//
// r.CloseOnRead sets whether the result is closed after the first read.
//
// The result is never automatically closed if in progress of reading an array.
func (r *Result) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	defer func() {
		if r.CloseOnRead && len(r.arrayStack) == 0 && !r.subscribeMode {
			r.close()
		}
	}()

	n, _, err := r.writeTo(w)
	if err != nil {
		return n, err
	}
	return n, nil
}

// ArrayStack returns the position of the result within an array.
//
// The returned slice must not be modified.
func (r *Result) ArrayStack() []int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.arrayStack
}

// Strings returns the array result as a slice of strings.
func (r *Result) Strings() ([]string, error) {
	// Strings intentionally doesn't hold the mutex or interact with other
	// internal fields to demonstrate how to use the public API.
	defer func() {
		if r.CloseOnRead {
			r.Close()
		}
	}()

	// Read the array length.
	ln, err := r.ArrayLength()
	if err != nil {
		return nil, fmt.Errorf("read array length: %w", err)
	}

	if ln < 0 {
		return nil, nil
	}

	var ss []string
	for i := 0; i < ln; i++ {
		s, err := r.String()
		if err != nil {
			return ss, fmt.Errorf("read string %d: %w", i, err)
		}
		ss = append(ss, s)
	}
	return ss, nil
}

type replyType byte

const (
	replyTypeSimpleString replyType = '+'
	replyTypeError        replyType = '-'
	replyTypeInteger      replyType = ':'
	replyTypeBulkString   replyType = '$'
	replyTypeArray        replyType = '*'
)

// writeTo writes the result to w. The second return value is whether or not
// the value indicates an array.
//
// It is the master function of the Result type, centralizing key logic.
func (r *Result) writeTo(w io.Writer) (int64, replyType, error) {
	if err := r.checkClosed(); err != nil {
		return 0, 0, err
	}

	if r.err != nil {
		return 0, 0, r.err
	}

	if r.pipeline.at == r.pipeline.end && len(r.arrayStack) == 0 && !r.subscribeMode {
		return 0, 0, fmt.Errorf("no more results")
	}

	r.err = r.conn.wr.Flush()
	if r.err != nil {
		r.err = fmt.Errorf("flush: %w", r.err)
		return 0, 0, r.err
	}

	var typByte byte
	typByte, r.err = r.conn.rd.ReadByte()
	if r.err != nil {
		r.err = fmt.Errorf("read type: %w", r.err)
		return 0, 0, r.err
	}
	typ := replyType(typByte)

	// incrRead is how we advance the read state machine.
	incrRead := func() {
		if len(r.arrayStack) == 0 {
			r.pipeline.at++
			return
		}
		// If we're in an array, we're not making progress on the inter-command pipeline,
		// we're just reading the array elements.
		i := len(r.arrayStack) - 1
		r.arrayStack[i] = r.arrayStack[i] - 1

		if r.arrayStack[i] == 0 {
			r.arrayStack = r.arrayStack[:i]
			// This was just cleanup, we move the pipeline forward.
			r.pipeline.at++
		}
	}

	var s []byte

	switch typ {
	case replyTypeSimpleString, replyTypeInteger, replyTypeArray:
		// Simple string or integer
		s, r.err = readUntilNewline(r.conn.rd, r.conn.miscBuf)
		if r.err != nil {
			return 0, typ, r.err
		}

		isNewArray := typ == '*'
		if isNewArray {
			// New array
			n, err := strconv.Atoi(string(s))
			if err != nil {
				return 0, typ, fmt.Errorf("invalid array length %q", s)
			}
			r.arrayStack = append(r.arrayStack, n)
		}

		var n int
		n, r.err = w.Write(s)
		if !isNewArray {
			incrRead()
		}
		return int64(n), typ, r.err
	case replyTypeBulkString:
		// Bulk string
		var n int
		n, r.err = readBulkString(w, r.conn, r.conn.rd, r.conn.miscBuf)
		incrRead()
		return int64(n), typ, r.err
	case replyTypeError:
		// Error
		s, r.err = readUntilNewline(r.conn.rd, r.conn.miscBuf)
		if r.err != nil {
			return 0, typ, r.err
		}
		incrRead()
		return 0, typ, &Error{
			raw: string(s),
		}
	default:
		r.err = fmt.Errorf("unknown type %q", typ)
		return 0, typ, r.err
	}
}

// Bytes returns the result as a byte slice.
//
// Refer to r.CloseOnRead for whether the result is closed after the first read.
func (r *Result) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	_, err := r.WriteTo(&buf)
	return buf.Bytes(), err
}

// String returns the result as a string.
//
// Refer to r.CloseOnRead for whether the result is closed after the first read.
func (r *Result) String() (string, error) {
	var sb strings.Builder
	_, err := r.WriteTo(&sb)
	return sb.String(), err
}

// Int returns the result as an integer.
//
// Refer to r.CloseOnRead for whether the result is closed after the first read.
func (r *Result) Int() (int, error) {
	s, err := r.String()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

// ArrayLength reads the next message as an array length.
// It does not close the Result even if CloseOnRead is true.
func (r *Result) ArrayLength() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var buf bytes.Buffer
	_, typ, err := r.writeTo(&buf)
	if err != nil {
		return 0, err
	}

	if typ != replyTypeArray {
		return 0, fmt.Errorf("expected array, got %q", typ)
	}

	gotN, err := strconv.Atoi(buf.String())
	if err != nil {
		return 0, fmt.Errorf("invalid array length %q", buf.String())
	}

	// -1 is a nil array.
	if gotN < 0 {
		return gotN, nil
	}

	if r.arrayStack[len(r.arrayStack)-1] != gotN {
		// This should be impossible.
		return 0, fmt.Errorf("array stack mismatch (expected %d, got %d)", r.arrayStack[len(r.arrayStack)-1], gotN)
	}

	return gotN, nil
}

// Ok returns whether the result is "OK". Note that it may fail even if the
//
// command succeeded. For example, a successful GET will return a value.
func (r *Result) Ok() error {
	got, err := r.Bytes()
	if err != nil {
		return err
	}
	if !bytes.Equal(got, []byte("OK")) {
		return fmt.Errorf("expected OK, got %q", got)
	}
	return nil
}

// Next returns true if there are more results to read.
func (r *Result) Next() bool {
	if r == nil {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.next()
}

// next returns true if there are more results to read.
func (r *Result) next() bool {
	if r.err != nil {
		return false
	}

	return r.pipeline.at < r.pipeline.end || len(r.arrayStack) > 0
}

// Close releases all resources associated with the result.
//
// It is safe to call Close multiple times.
func (r *Result) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.close()
}

func (r *Result) close() error {
	for r.next() {
		// Read the result into discard so that the connection can be reused.
		_, _, err := r.writeTo(io.Discard)
		if errors.Is(err, errClosed) {
			// Should be impossible to close a result without draining
			// it, in which case at == end and we would never get here.
			return fmt.Errorf("SEVERE: result closed while iterating")
		} else if err != nil {
			return fmt.Errorf("drain: %w", err)
		}
	}

	if !atomic.CompareAndSwapInt64(&r.closed, 0, 1) {
		// double-close
		return nil
	}

	if r.closeCh != nil {
		close(r.closeCh)
	}

	conn := r.conn
	// r.conn is set to nil to prevent accidental reuse.
	r.conn = nil
	if r.err == nil && !r.subscribeMode {
		r.client.putConn(conn)
		return nil
	}

	if conn != nil {
		return conn.Close()
	}

	return nil
}
