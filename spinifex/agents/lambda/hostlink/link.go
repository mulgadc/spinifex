// Package hostlink is the guest's boundary to host control: an authenticated
// TCP connection under per-generation mutual TLS. The message protocol over it
// is not yet chosen; implementations of Link are carriers.
package hostlink

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
	// Credential is the environment's execution-role session. Only test-link
	// hosts send it; release hosts leave it nil.
	Credential *Credential `json:"credential,omitempty"`
}

// Credential holds one execution-role session and its function environment.
type Credential struct {
	AccessKeyID     string            `json:"access_key_id"`
	SecretAccessKey string            `json:"secret_access_key"`
	SessionToken    string            `json:"session_token"`
	Environment     map[string]string `json:"environment,omitempty"`
}

// LogValue keeps every value out of logs.
func (Credential) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// Complete reports whether all three values are present.
func (c Credential) Complete() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != "" && c.SessionToken != ""
}

// Response is the outcome of a Request. FunctionError is empty on success. A
// non-nil Segments is the payload and takes precedence over Payload; only a
// streaming carrier can send it, and encoding/json refuses it.
type Response struct {
	GrantID       string   `json:"grant_id"`
	AttemptID     string   `json:"attempt_id"`
	RequestID     string   `json:"request_id"`
	Payload       []byte   `json:"payload"`
	Segments      [][]byte `json:"-"`
	FunctionError string   `json:"function_error,omitempty"`
}

// ErrSegmentedNotMarshalable is returned when a segmented payload reaches
// encoding/json, which would otherwise drop it.
var ErrSegmentedNotMarshalable = errors.New("hostlink: a segmented payload can only be streamed by a carrier")

type responseAlias Response

// MarshalJSON encodes r as the plain struct, and fails for a segmented
// payload rather than emit a frame without it.
func (r Response) MarshalJSON() ([]byte, error) {
	if r.Segments != nil {
		return nil, ErrSegmentedNotMarshalable
	}
	return json.Marshal(responseAlias(r))
}

// Link carries invocations from the host to the guest and results back.
type Link interface {
	// Next blocks for the next request, or returns ErrShutdown.
	Next(ctx context.Context) (Request, error)
	// Complete reports the outcome of a request.
	Complete(ctx context.Context, resp Response) error
}
