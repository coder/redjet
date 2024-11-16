package redjet

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	return "server: " + e.raw
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

// Pipeline is the result of a command.
//
// Its methods are not safe for concurrent use.
type Pipeline struct {
	// CloseOnRead determines whether the Pipeline is closed after the first read.
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

	// protoErr is set to non-nil if there is an unrecoverable protocol error.
	protoErr error

	conn   *conn
	client *Client

	pipeline pipeline

	// arrayStack tracks the depth of the array processing. E.g. if we're one
	// level deep with 3 elements remaining, arrayStack will be [3].
	arrayStack []int
}

func (r *Pipeline) Error() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.protoErr == nil {
		return ""
	}
	return r.protoErr.Error()
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

// ErrNil is a nil value. For example, it is returned for missing keys in
// GET and MGET.
var ErrNil = errors.New("(nil)")

func readBulkString(w io.Writer, rd *bufio.Reader, copyBuf []byte) (int, error) {
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
		return 0, ErrNil
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

func (r *Pipeline) checkClosed() error {
	if atomic.LoadInt64(&r.closed) != 0 {
		return errClosed
	}
	return nil
}

var _ io.WriterTo = (*Pipeline)(nil)

// WriteTo writes the result to w.
//
// r.CloseOnRead sets whether the result is closed after the first read.
//
// The result is never automatically closed if in progress of reading an array.
func (r *Pipeline) WriteTo(w io.Writer) (int64, error) {
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
func (r *Pipeline) ArrayStack() []int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.arrayStack
}

// Strings returns the array result as a slice of strings.
func (r *Pipeline) Strings() ([]string, error) {
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

	if ln <= 0 {
		return nil, nil
	}

	var ss []string
	for i := 0; i < ln; i++ {
		s, err := r.String()
		if err != nil && !errors.Is(err, ErrNil) {
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
// It is the master function of the Pipeline type, centralizing key logic.
func (r *Pipeline) writeTo(w io.Writer) (int64, replyType, error) {
	if err := r.checkClosed(); err != nil {
		return 0, 0, err
	}

	if r.protoErr != nil {
		return 0, 0, r.protoErr
	}

	if r.pipeline.at == r.pipeline.end && len(r.arrayStack) == 0 && !r.subscribeMode {
		return 0, 0, fmt.Errorf("no more results")
	}

	r.protoErr = r.conn.wr.Flush()
	if r.protoErr != nil {
		r.protoErr = fmt.Errorf("flush: %w", r.protoErr)
		return 0, 0, r.protoErr
	}

	var typByte byte
	typByte, r.protoErr = r.conn.rd.ReadByte()
	if r.protoErr != nil {
		r.protoErr = fmt.Errorf("read type: %w", r.protoErr)
		return 0, 0, r.protoErr
	}
	typ := replyType(typByte)

	// incrRead is how we advance the read state machine.
	incrRead := func(isNewArray bool) {
		if len(r.arrayStack) == 0 {
			r.pipeline.at++
			return
		}
		// If we're in an array, we're not making progress on the inter-command pipeline,
		// we're just reading the array elements.
		i := len(r.arrayStack) - 1
		r.arrayStack[i] = r.arrayStack[i] - 1

		// We don't do this cleanup on new arrays so that
		// we can support stacks like [0, 2] for the final
		// list in a series of lists.
		if r.arrayStack[i] == 0 && !isNewArray {
			r.arrayStack = r.arrayStack[:i]
			// This was just cleanup, we move the pipeline forward.
			r.pipeline.at++
		}
	}

	var s []byte

	switch typ {
	case replyTypeSimpleString, replyTypeInteger, replyTypeArray:
		// Simple string or integer
		s, r.protoErr = readUntilNewline(r.conn.rd, r.conn.miscBuf)
		if r.protoErr != nil {
			return 0, typ, r.protoErr
		}

		isNewArray := typ == '*'

		var n int
		n, r.protoErr = w.Write(s)
		incrRead(isNewArray)
		var newArraySize int
		if isNewArray {
			var err error
			// New array
			newArraySize, err = strconv.Atoi(string(s))
			if err != nil {
				return 0, typ, fmt.Errorf("invalid array length %q", s)
			}
			if newArraySize > 0 {
				r.arrayStack = append(r.arrayStack, newArraySize)
			}
		}
		return int64(n), typ, r.protoErr
	case replyTypeBulkString:
		// Bulk string
		var (
			n   int
			err error
		)
		n, err = readBulkString(w, r.conn.rd, r.conn.miscBuf)
		incrRead(false)
		// A nil is highly recoverable.
		if !errors.Is(err, ErrNil) {
			r.protoErr = err
		}
		return int64(n), typ, err
	case replyTypeError:
		// Error
		s, r.protoErr = readUntilNewline(r.conn.rd, r.conn.miscBuf)
		if r.protoErr != nil {
			return 0, typ, r.protoErr
		}
		incrRead(false)
		return 0, typ, &Error{
			raw: string(s),
		}
	default:
		r.protoErr = fmt.Errorf("unknown type %q", typ)
		return 0, typ, r.protoErr
	}
}

// Bytes returns the result as a byte slice.
//
// Refer to r.CloseOnRead for whether the result is closed after the first read.
func (r *Pipeline) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	_, err := r.WriteTo(&buf)
	return buf.Bytes(), err
}

// JSON unmarshals the result into v.
func (r *Pipeline) JSON(v interface{}) error {
	b, err := r.Bytes()
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// String returns the result as a string.
//
// Refer to r.CloseOnRead for whether the result is closed after the first read.
func (r *Pipeline) String() (string, error) {
	var sb strings.Builder
	_, err := r.WriteTo(&sb)
	return sb.String(), err
}

// Int returns the result as an integer.
//
// Refer to r.CloseOnRead for whether the result is closed after the first read.
func (r *Pipeline) Int() (int, error) {
	s, err := r.String()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

// ArrayLength reads the next message as an array length.
// It does not close the Pipeline even if CloseOnRead is true.
func (r *Pipeline) ArrayLength() (int, error) {
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
	if gotN <= 0 {
		return gotN, nil
	}

	if len(r.arrayStack) == 0 {
		return 0, fmt.Errorf("bug: array stack not set")
	}

	// Sanity check that we've populated the array stack correctly.
	if r.arrayStack[len(r.arrayStack)-1] != gotN {
		// This should be impossible.
		return 0, fmt.Errorf("array stack mismatch (expected %d, got %d)", r.arrayStack[len(r.arrayStack)-1], gotN)
	}

	return gotN, nil
}

// Ok returns whether the result is "OK". Note that it may fail even if the
//
// command succeeded. For example, a successful GET will return a value.
func (r *Pipeline) Ok() error {
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
func (r *Pipeline) Next() bool {
	if r == nil {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	return r.HasMore()
}

// HasMore returns true if there are more results to read.
func (r *Pipeline) HasMore() bool {
	if r.protoErr != nil {
		return false
	}

	var arrays int
	for _, n := range r.arrayStack {
		arrays += n
	}

	return r.pipeline.at < r.pipeline.end || arrays > 0
}

// Close releases all resources associated with the result.
//
// It is safe to call Close multiple times.
func (r *Pipeline) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.close()
}

func (r *Pipeline) close() error {
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
	// Only return conn when it is in a known good state.
	if r.protoErr == nil && !r.subscribeMode && !r.HasMore() {
		r.client.putConn(conn)
		return nil
	}

	if conn != nil {
		return conn.Close()
	}

	return nil
}
