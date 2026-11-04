// Package runtimeapi serves the AWS Lambda Runtime API (version 2018-06-01)
// inside the guest, handing one invocation at a time to the function runtime.
package runtimeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// MaxResponseBytes is the largest response payload a runtime may post.
const MaxResponseBytes = 6291456

const (
	// ErrTypeResponseTooLarge is the function error for an oversize response.
	ErrTypeResponseTooLarge = "Function.ResponseSizeTooLarge"
	// ErrTypeUnhandled is the function error when the runtime names no type.
	ErrTypeUnhandled = "Unhandled"

	errTypeInvalidRequestID = "InvalidRequestID"
	errTypeTooLarge         = "RequestEntityTooLarge"
	errTypeInvalidRequest   = "InvalidRequest"

	headerRequestID = "Lambda-Runtime-Aws-Request-Id"
	headerDeadline  = "Lambda-Runtime-Deadline-Ms"
	headerARN       = "Lambda-Runtime-Invoked-Function-Arn"
	headerTraceID   = "Lambda-Runtime-Trace-Id"
	headerErrorType = "Lambda-Runtime-Function-Error-Type"

	maxInitErrorBytes = 1 << 20
)

var (
	// ErrBusy is returned by Invoke while another invocation is in flight.
	ErrBusy = errors.New("runtimeapi: an invocation is already in flight")
	// ErrDeadlineExceeded is returned by Invoke when the deadline passes.
	ErrDeadlineExceeded = errors.New("runtimeapi: invocation deadline exceeded")
	// ErrInvalidInvocation is returned by Invoke for an invocation with no request ID.
	ErrInvalidInvocation = errors.New("runtimeapi: invocation has no request id")
	// ErrResponseBodyFailed is returned by Invoke when a response or error
	// body that was admitted could not be read in full. No result exists.
	ErrResponseBodyFailed = errors.New("runtimeapi: admitted response body was not received")
)

// Invocation is one event handed to the runtime.
type Invocation struct {
	RequestID   string
	FunctionARN string
	Deadline    time.Time
	TraceID     string
	Payload     []byte
}

// Result is what the runtime answered. FunctionError is empty on success and
// otherwise the error type. A non-nil Segments is the payload, in order, and
// takes precedence over Payload.
type Result struct {
	Payload       []byte
	Segments      [][]byte
	FunctionError string
}

// InitError is an initialisation failure reported by the runtime.
type InitError struct {
	Type string
	Body []byte
}

type flightState int

const (
	stWaiting flightState = iota
	stRunning
	stReceiving
	stCompleted
	stExited
)

type outcome struct {
	res Result
	err error
}

type flight struct {
	inv   Invocation
	state flightState
	done  chan outcome
}

// Server implements the Runtime API for a single in-flight invocation.
type Server struct {
	mu   sync.Mutex
	cur  *flight
	wake chan struct{}

	// exited records that the bootstrap process has ended; settled that the
	// last flight has its result, so a later exit no longer matters.
	exited  bool
	settled bool

	initOnce sync.Once
	initDone chan struct{}
	initErr  InitError
}

// New returns a Server with no invocation in flight.
func New() *Server {
	return &Server{wake: make(chan struct{}, 1), initDone: make(chan struct{})}
}

// InitFailed is closed once the runtime reports an init error.
func (s *Server) InitFailed() <-chan struct{} { return s.initDone }

// InitError returns the reported init error, if any.
func (s *Server) InitError() (InitError, bool) {
	select {
	case <-s.initDone:
		return s.initErr, true
	default:
		return InitError{}, false
	}
}

// Invoke hands inv to the runtime and blocks until the runtime posts a
// response or error, the deadline passes, or ctx ends.
func (s *Server) Invoke(ctx context.Context, inv Invocation) (Result, error) {
	if inv.RequestID == "" {
		return Result{}, ErrInvalidInvocation
	}
	f := &flight{inv: inv, done: make(chan outcome, 1)}
	s.mu.Lock()
	if s.cur != nil {
		s.mu.Unlock()
		return Result{}, ErrBusy
	}
	if s.exited {
		f.state = stExited
	}
	s.settled = false
	s.cur = f
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}

	timer := time.NewTimer(time.Until(inv.Deadline))
	defer timer.Stop()
	var cause error
	select {
	case out := <-f.done:
		return out.res, out.err
	case <-timer.C:
		cause = ErrDeadlineExceeded
	case <-ctx.Done():
		cause = ctx.Err()
	}

	s.mu.Lock()
	if s.cur == f {
		s.cur = nil
		s.mu.Unlock()
		return Result{}, cause
	}
	s.mu.Unlock()
	// Completed while we were giving up; the result is already queued.
	out := <-f.done
	return out.res, out.err
}

// BootstrapExited records that the runtime process has ended and reports
// whether the caller should cancel the invocation. Before the response is
// admitted it should; afterwards the result stands.
func (s *Server) BootstrapExited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exited = true
	f := s.cur
	switch {
	case f == nil:
		return !s.settled
	case f.state < stReceiving:
		f.state = stExited
		return true
	default:
		return false
	}
}

// Handler returns the Runtime API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /2018-06-01/runtime/invocation/next", s.next)
	mux.HandleFunc("POST /2018-06-01/runtime/invocation/{id}/response", s.response)
	mux.HandleFunc("POST /2018-06-01/runtime/invocation/{id}/error", s.invocationError)
	mux.HandleFunc("POST /2018-06-01/runtime/init/error", s.initError)
	return mux
}

func (s *Server) next(w http.ResponseWriter, r *http.Request) {
	for {
		s.mu.Lock()
		f := s.cur
		if f != nil && f.state == stWaiting {
			f.state = stRunning
			s.mu.Unlock()
			h := w.Header()
			h.Set(headerRequestID, f.inv.RequestID)
			h.Set(headerDeadline, strconv.FormatInt(f.inv.Deadline.UnixMilli(), 10))
			h.Set(headerARN, f.inv.FunctionARN)
			if f.inv.TraceID != "" {
				h.Set(headerTraceID, f.inv.TraceID)
			}
			_, _ = w.Write(f.inv.Payload)
			return
		}
		s.mu.Unlock()
		select {
		case <-s.wake:
		case <-r.Context().Done():
			return
		}
	}
}

// admit claims the single response slot of the invocation named id, so only
// one POST per invocation is ever read. It returns nil if id is not the
// delivered, unclaimed invocation of a runtime that is still running.
func (s *Server) admit(id string) *flight {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.cur
	if f == nil || f.state != stRunning || f.inv.RequestID != id {
		return nil
	}
	f.state = stReceiving
	return f
}

// complete records res for an admitted flight, reporting whether the
// invocation was still waiting for it.
func (s *Server) complete(f *flight, res Result) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != f {
		return false
	}
	s.cur = nil
	s.settled = true
	f.state = stCompleted
	f.done <- outcome{res: res}
	return true
}

// readFailed ends an admitted flight whose body was lost. The outcome is
// published before the flight is detached, so Invoke never waits on it.
func (s *Server) readFailed(f *flight, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != f {
		return
	}
	s.cur = nil
	s.settled = true
	f.state = stCompleted
	f.done <- outcome{err: fmt.Errorf("%w: %w", ErrResponseBodyFailed, err)}
}

func (s *Server) response(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f := s.admit(id)
	if f == nil {
		invalidID(w, id)
		return
	}
	s.finishResponse(w, r, f)
}

// segmentBytes is the largest piece an undeclared-length body is read into.
const segmentBytes = 256 << 10

// finishResponse reads the admitted response body and completes f.
func (s *Server) finishResponse(w http.ResponseWriter, r *http.Request, f *flight) {
	var res Result
	var oversize bool
	var err error
	switch cl := r.ContentLength; {
	case cl > MaxResponseBytes:
		oversize, err = discardToCap(r.Body)
	case cl >= 0:
		res.Payload = make([]byte, cl)
		_, err = io.ReadFull(r.Body, res.Payload)
	default:
		res.Segments, oversize, err = readSegments(r.Body)
	}
	if err != nil {
		s.readFailed(f, err)
		writeError(w, http.StatusBadRequest, errTypeInvalidRequest, "read body: "+err.Error())
		return
	}
	if oversize {
		msg := "Response payload size exceeded maximum allowed payload size (" + strconv.Itoa(MaxResponseBytes) + " bytes)."
		s.complete(f, Result{Payload: ErrorBody(msg, ErrTypeResponseTooLarge), FunctionError: ErrTypeUnhandled})
		writeError(w, http.StatusRequestEntityTooLarge, errTypeTooLarge, msg)
		return
	}
	if !s.complete(f, res) {
		invalidID(w, f.inv.RequestID)
		return
	}
	writeStatus(w)
}

// discardToCap counts body through a small buffer. It reports oversize once
// byte MaxResponseBytes+1 arrives and an error if the body ends first.
func discardToCap(body io.Reader) (bool, error) {
	n, err := io.CopyN(io.Discard, body, MaxResponseBytes+1)
	if n == MaxResponseBytes+1 {
		return true, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return false, err
}

// readSegments reads body into segments of at most segmentBytes, never
// concatenating them. It reports oversize and drops them at byte
// MaxResponseBytes+1.
func readSegments(body io.Reader) ([][]byte, bool, error) {
	segs := [][]byte{}
	total := 0
	for {
		n := min(segmentBytes, MaxResponseBytes+1-total)
		seg := make([]byte, n)
		got := 0
		var err error
		for got < n && err == nil {
			var c int
			c, err = body.Read(seg[got:])
			got += c
		}
		total += got
		if total > MaxResponseBytes {
			return nil, true, nil
		}
		if got > 0 {
			segs = append(segs, seg[:got])
		}
		if errors.Is(err, io.EOF) {
			return segs, false, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
}

func (s *Server) invocationError(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f := s.admit(id)
	if f == nil {
		invalidID(w, id)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxResponseBytes))
	if err != nil {
		s.readFailed(f, err)
		writeError(w, http.StatusBadRequest, errTypeInvalidRequest, "read body: "+err.Error())
		return
	}
	errType := r.Header.Get(headerErrorType)
	if errType == "" {
		errType = ErrTypeUnhandled
	}
	if !s.complete(f, Result{Payload: body, FunctionError: errType}) {
		invalidID(w, id)
		return
	}
	writeStatus(w)
}

func (s *Server) initError(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxInitErrorBytes))
	s.initOnce.Do(func() {
		s.initErr = InitError{Type: r.Header.Get(headerErrorType), Body: body}
		close(s.initDone)
	})
	writeStatus(w)
}

func invalidID(w http.ResponseWriter, id string) {
	writeError(w, http.StatusBadRequest, errTypeInvalidRequestID, "Invalid request ID: "+id)
}

func writeStatus(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"OK"}`))
}

func writeError(w http.ResponseWriter, code int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(ErrorBody(msg, errType))
}

type errorPayload struct {
	ErrorMessage string `json:"errorMessage"`
	ErrorType    string `json:"errorType"`
}

// ErrorBody renders the Lambda error document for msg and errType.
func ErrorBody(msg, errType string) []byte {
	b, err := json.Marshal(errorPayload{ErrorMessage: msg, ErrorType: errType})
	if err != nil {
		return []byte(`{"errorType":"Runtime.Unknown"}`)
	}
	return b
}
