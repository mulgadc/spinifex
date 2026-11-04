//go:build lambdatestlink

package hostlink

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"
)

// maxFrame bounds one frame: a 6 MiB payload as base64 plus headroom.
const maxFrame = (6<<20+2)/3*4 + 64<<10

const (
	typeInvoke   = "invoke"
	typeShutdown = "shutdown"
	typeResult   = "result"
)

// ErrFrameTooLarge is returned for a frame beyond the size bound.
var ErrFrameTooLarge = errors.New("hostlink: frame too large")

type frame struct {
	Type     string    `json:"type"`
	Request  *Request  `json:"request,omitempty"`
	Response *Response `json:"response,omitempty"`
}

// TestStream is a test-only carrier over an io.ReadWriter, framing JSON with
// a 4-byte big-endian length. It is not the guest protocol.
type TestStream struct {
	rw    io.ReadWriter
	conn  net.Conn // rw when it is a net.Conn; results are written only then
	in    chan frame
	wslot chan struct{}

	// closed is set, under the write slot, once the connection was closed
	// with a result possibly half sent.
	closed atomic.Bool

	err error // set before in is closed
}

var _ Link = (*TestStream)(nil)

// NewTestStream starts reading frames from rw.
func NewTestStream(rw io.ReadWriter) *TestStream {
	// One slot lets the reader finish after an unread frame instead of blocking.
	s := &TestStream{rw: rw, in: make(chan frame, 1), wslot: make(chan struct{}, 1)}
	s.conn, _ = rw.(net.Conn)
	go s.readLoop()
	return s
}

func (s *TestStream) readLoop() {
	defer close(s.in)
	for {
		f, err := readFrame(s.rw)
		if err != nil {
			s.err = err
			return
		}
		s.in <- f
	}
}

func (s *TestStream) recv(ctx context.Context) (frame, error) {
	select {
	case f, ok := <-s.in:
		if !ok {
			return frame{}, s.err
		}
		return f, nil
	case <-ctx.Done():
		return frame{}, ctx.Err()
	}
}

// Next implements Link.
func (s *TestStream) Next(ctx context.Context) (Request, error) {
	f, err := s.recv(ctx)
	if err != nil {
		return Request{}, err
	}
	switch f.Type {
	case typeShutdown:
		return Request{}, ErrShutdown
	case typeInvoke:
		if f.Request == nil {
			return Request{}, errors.New("hostlink: invoke frame without request")
		}
		return *f.Request, nil
	default:
		return Request{}, fmt.Errorf("hostlink: unexpected frame type %q", f.Type)
	}
}

// ErrNoDeadlineTransport is returned by Complete when the stream's transport
// is not a net.Conn, since a blocked write could then never be interrupted.
var ErrNoDeadlineTransport = errors.New("hostlink: result needs a net.Conn transport")

// ErrStreamClosed is returned by Complete after a failed write closed the
// connection.
var ErrStreamClosed = errors.New("hostlink: stream closed after a failed result write")

// pastDeadline is a write deadline that is always already over.
var pastDeadline = time.Unix(1, 0)

// acquire takes the write slot or gives up when ctx ends.
func (s *TestStream) acquire(ctx context.Context) error {
	select {
	case s.wslot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *TestStream) release() { <-s.wslot }

// Complete implements Link. The result is streamed as the same JSON frame
// json.Marshal makes for it, without holding a copy of the payload.
func (s *TestStream) Complete(ctx context.Context, resp Response) (err error) {
	if s.conn == nil {
		return ErrNoDeadlineTransport
	}
	if s.closed.Load() {
		return ErrStreamClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	rf, err := newResultFrame(resp)
	if err != nil {
		return err
	}
	if rf.size > maxFrame {
		return ErrFrameTooLarge
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	// The slot is released last, after the deadline has been cleared.
	defer s.release()
	if s.closed.Load() {
		return ErrStreamClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Once any frame byte may be out, the connection is closed before the
	// slot is released so no later writer can append to a partial frame.
	closeConn := func() {
		s.closed.Store(true)
		_ = s.conn.Close()
	}

	dl, _ := ctx.Deadline()
	if err := s.conn.SetWriteDeadline(dl); err != nil {
		closeConn()
		return fmt.Errorf("hostlink: set write deadline: %w", err)
	}
	stop, watched, watchErr := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		defer close(watched)
		select {
		case <-ctx.Done():
			if err := s.conn.SetWriteDeadline(pastDeadline); err != nil {
				// Closing is what unblocks the pending write.
				s.closed.Store(true)
				_ = s.conn.Close()
				watchErr <- err
			}
		case <-stop:
		}
	}()
	cw := &countingWriter{w: s.conn}
	werr := rf.writeTo(cw)
	close(stop)
	<-watched
	select {
	case e := <-watchErr:
		return fmt.Errorf("hostlink: interrupt blocked write: %w", errors.Join(e, werr))
	default:
	}
	if werr == nil && cw.n != int64(4+rf.size) {
		werr = fmt.Errorf("hostlink: frame wrote %d bytes, expected %d", cw.n, 4+rf.size)
	}
	if werr != nil {
		closeConn()
		return werr
	}
	if cerr := s.conn.SetWriteDeadline(time.Time{}); cerr != nil {
		closeConn()
		return fmt.Errorf("hostlink: clear write deadline: %w", errors.Join(cerr, werr))
	}
	return nil
}

// countingWriter writes everything to w, in full, and counts the bytes.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	done := 0
	for done < len(p) {
		n, err := c.w.Write(p[done:])
		done += n
		c.n += int64(n)
		if err != nil {
			return done, err
		}
		if n == 0 {
			return done, io.ErrShortWrite
		}
	}
	return done, nil
}

// resultFrame is the result frame in the field order json.Marshal gives
// frame{Type, Response}, split around the payload value.
type resultFrame struct {
	head, tail []byte
	payload    [][]byte
	null       bool
	size       int // JSON length, excluding the 4-byte prefix
}

func newResultFrame(r Response) (*resultFrame, error) {
	q := func(v string) ([]byte, error) { return json.Marshal(v) }
	var parts [3][]byte
	for i, v := range []string{r.GrantID, r.AttemptID, r.RequestID} {
		b, err := q(v)
		if err != nil {
			return nil, err
		}
		parts[i] = b
	}
	rf := &resultFrame{}
	rf.head = append(rf.head, `{"type":"result","response":{"grant_id":`...)
	rf.head = append(rf.head, parts[0]...)
	rf.head = append(rf.head, `,"attempt_id":`...)
	rf.head = append(rf.head, parts[1]...)
	rf.head = append(rf.head, `,"request_id":`...)
	rf.head = append(rf.head, parts[2]...)
	rf.head = append(rf.head, `,"payload":`...)
	if r.FunctionError != "" {
		b, err := q(r.FunctionError)
		if err != nil {
			return nil, err
		}
		rf.tail = append(rf.tail, `,"function_error":`...)
		rf.tail = append(rf.tail, b...)
	}
	rf.tail = append(rf.tail, `}}`...)

	total := 0
	switch {
	case r.Segments != nil:
		rf.payload = r.Segments
	case r.Payload != nil:
		rf.payload = [][]byte{r.Payload}
	default:
		rf.null = true
	}
	for _, seg := range rf.payload {
		total += len(seg)
	}
	rf.size = len(rf.head) + len(rf.tail)
	if rf.null {
		rf.size += len("null")
	} else {
		// A payload too large for maxFrame is refused before its length matters.
		if total > maxFrame {
			rf.size = maxFrame + 1
			return rf, nil
		}
		rf.size += 2 + base64.StdEncoding.EncodedLen(total)
	}
	return rf, nil
}

// writeTo writes the length prefix and the frame, in order, through a small
// buffer so the payload is never held a second time.
func (rf *resultFrame) writeTo(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 32<<10)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(rf.size))
	_, _ = bw.Write(hdr[:])
	_, _ = bw.Write(rf.head)
	if rf.null {
		_, _ = bw.WriteString("null")
	} else {
		_ = bw.WriteByte('"')
		enc := base64.NewEncoder(base64.StdEncoding, bw)
		for _, seg := range rf.payload {
			if _, err := enc.Write(seg); err != nil {
				return err
			}
		}
		if err := enc.Close(); err != nil {
			return err
		}
		_ = bw.WriteByte('"')
	}
	_, _ = bw.Write(rf.tail)
	return bw.Flush()
}

// HostSend writes an invoke frame, as the host side of the carrier.
func (s *TestStream) HostSend(req Request) error {
	return s.write(frame{Type: typeInvoke, Request: &req})
}

// HostShutdown writes a shutdown frame, as the host side of the carrier.
func (s *TestStream) HostShutdown() error {
	return s.write(frame{Type: typeShutdown})
}

// HostResult reads a result frame, as the host side of the carrier.
func (s *TestStream) HostResult(ctx context.Context) (Response, error) {
	f, err := s.recv(ctx)
	if err != nil {
		return Response{}, err
	}
	if f.Type != typeResult || f.Response == nil {
		return Response{}, fmt.Errorf("hostlink: unexpected frame type %q", f.Type)
	}
	return *f.Response, nil
}

func (s *TestStream) write(f frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_ = s.acquire(context.Background())
	defer s.release()
	_, err = s.rw.Write(buf)
	return err
}

func readFrame(r io.Reader) (frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return frame{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return frame{}, ErrFrameTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return frame{}, err
	}
	var f frame
	if err := json.Unmarshal(body, &f); err != nil {
		return frame{}, fmt.Errorf("hostlink: bad frame: %w", err)
	}
	return f, nil
}
