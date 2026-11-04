// Package runtimeapi serves the AWS Lambda Runtime API (version 2018-06-01)
// inside the guest, handing one invocation at a time to the function runtime.
package runtimeapi

import (
	"context"
	"encoding/json"
	"errors"
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
// otherwise the error type.
type Result struct {
	Payload       []byte
	FunctionError string
}

// InitError is an initialisation failure reported by the runtime.
type InitError struct {
	Type string
	Body []byte
}

type flight struct {
	inv   Invocation
	taken bool
	done  chan Result
}

// Server implements the Runtime API for a single in-flight invocation.
type Server struct {
	mu   sync.Mutex
	cur  *flight
	wake chan struct{}

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
	f := &flight{inv: inv, done: make(chan Result, 1)}
	s.mu.Lock()
	if s.cur != nil {
		s.mu.Unlock()
		return Result{}, ErrBusy
	}
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
	case res := <-f.done:
		return res, nil
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
	return <-f.done, nil
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
		if f != nil && !f.taken {
			f.taken = true
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

// claimable reports whether id names the delivered, uncompleted invocation.
func (s *Server) claimable(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil && s.cur.taken && s.cur.inv.RequestID == id
}

// complete finishes the invocation named id, reporting whether it was live.
func (s *Server) complete(id string, res Result) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.cur
	if f == nil || !f.taken || f.inv.RequestID != id {
		return false
	}
	s.cur = nil
	f.done <- res
	return true
}

func (s *Server) response(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.claimable(id) {
		invalidID(w, id)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxResponseBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, errTypeInvalidRequest, "read body: "+err.Error())
		return
	}
	if len(body) > MaxResponseBytes {
		msg := "Response payload size exceeded maximum allowed payload size (" + strconv.Itoa(MaxResponseBytes) + " bytes)."
		s.complete(id, Result{Payload: ErrorBody(msg, ErrTypeResponseTooLarge), FunctionError: ErrTypeUnhandled})
		writeError(w, http.StatusRequestEntityTooLarge, errTypeTooLarge, msg)
		return
	}
	if !s.complete(id, Result{Payload: body}) {
		invalidID(w, id)
		return
	}
	writeStatus(w)
}

func (s *Server) invocationError(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.claimable(id) {
		invalidID(w, id)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxResponseBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, errTypeInvalidRequest, "read body: "+err.Error())
		return
	}
	errType := r.Header.Get(headerErrorType)
	if errType == "" {
		errType = ErrTypeUnhandled
	}
	if !s.complete(id, Result{Payload: body, FunctionError: errType}) {
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
