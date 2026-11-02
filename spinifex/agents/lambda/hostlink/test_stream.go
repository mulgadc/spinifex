//go:build lambdatestlink

package hostlink

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
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
	rw  io.ReadWriter
	in  chan frame
	wmu sync.Mutex

	err error // set before in is closed
}

var _ Link = (*TestStream)(nil)

// NewTestStream starts reading frames from rw.
func NewTestStream(rw io.ReadWriter) *TestStream {
	s := &TestStream{rw: rw, in: make(chan frame)}
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

// Complete implements Link.
func (s *TestStream) Complete(ctx context.Context, resp Response) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.write(frame{Type: typeResult, Response: &resp})
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
	s.wmu.Lock()
	defer s.wmu.Unlock()
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
