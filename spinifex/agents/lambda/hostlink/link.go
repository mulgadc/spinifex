// Package hostlink is the guest's boundary to host control: an authenticated
// TCP connection under per-generation mutual TLS. The message protocol over it
// is not yet chosen; implementations of Link are carriers.
package hostlink

import (
	"context"
	"errors"
)

// ErrShutdown is returned by Link.Next when the host asks the agent to stop.
var ErrShutdown = errors.New("hostlink: host requested shutdown")

// Request is one invocation from the host. GrantID and AttemptID are opaque
// to the agent and are echoed unchanged in the Response.
type Request struct {
	GrantID     string `json:"grant_id"`
	AttemptID   string `json:"attempt_id"`
	RequestID   string `json:"request_id"`
	FunctionARN string `json:"function_arn"`
	TraceID     string `json:"trace_id,omitempty"`
	DeadlineMs  int64  `json:"deadline_ms"`
	Payload     []byte `json:"payload"`
}

// Response is the outcome of a Request. FunctionError is empty on success.
type Response struct {
	GrantID       string `json:"grant_id"`
	AttemptID     string `json:"attempt_id"`
	RequestID     string `json:"request_id"`
	Payload       []byte `json:"payload"`
	FunctionError string `json:"function_error,omitempty"`
}

// Link carries invocations from the host to the guest and results back.
type Link interface {
	// Next blocks for the next request, or returns ErrShutdown.
	Next(ctx context.Context) (Request, error)
	// Complete reports the outcome of a request.
	Complete(ctx context.Context, resp Response) error
}
