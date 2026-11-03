package awsapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// maxSyncPayload is the largest synchronous request payload AWS accepts.
const maxSyncPayload = 6 * 1024 * 1024

// waitOverhead is how much longer than the function timeout a request waits
// for its result. It bounds the wait only, never the execution.
const waitOverhead = 60 * time.Second

// SyncInvocation is a RequestResponse request handed to the execution owner,
// which decides admission.
type SyncInvocation struct {
	// Function and Revision are the one resolved configuration the attempt
	// binds; the environment stays sealed in Function.EnvCipher.
	Function function.Record
	Revision uint64
	Payload  []byte
	// WaitUntil is when the request stops waiting. Code may still be running
	// after it; it proves nothing about stop and releases no slot.
	WaitUntil time.Time
}

// SyncResult is what the function returned. FunctionError is "" on success;
// ExecutedVersion is the version that ran.
type SyncResult struct {
	Payload         []byte
	FunctionError   string
	ExecutedVersion string
}

// Executor admits and runs one synchronous attempt. Its context ends only at
// WaitUntil, never on caller disconnect. A *RefusalError means no code was
// dispatched; any other error means the outcome is unknown.
type Executor interface {
	InvokeSync(ctx context.Context, inv SyncInvocation) (SyncResult, error)
}

// RefusalError is a known pre-dispatch refusal, such as a capacity throttle. Err
// is the AWS error returned to the caller.
type RefusalError struct{ Err error }

func (r *RefusalError) Error() string { return "invocation refused before dispatch: " + r.Err.Error() }

func (r *RefusalError) Unwrap() error { return r.Err }

// invoke implements Invoke for RequestResponse on $LATEST.
func (d Deps) invoke(ctx context.Context, c *call) (reply, error) {
	if err := checkInvokeOptions(c.Header); err != nil {
		return reply{}, err
	}
	if len(c.Body) > maxSyncPayload {
		return reply{}, awserrors.Errorf(awserrors.ErrorRequestEntityTooLarge,
			"Request must be smaller than %d bytes for the InvokeFunction operation", maxSyncPayload)
	}
	rec, revision, err := d.lookup(ctx, c, c.Query.Get("Qualifier"))
	if err != nil {
		return reply{}, err
	}
	if rec.State != function.StateActive {
		return reply{}, awserrors.Errorf(awserrors.ErrorResourceConflictException,
			"The operation cannot be performed at this time. The function is currently in the following state: %s", rec.State)
	}
	if d.Exec == nil || rec.Code.State != function.CodeStored {
		return reply{}, serviceError("function execution is unavailable")
	}

	if err := ctx.Err(); err != nil {
		return reply{}, err
	}
	// Once accepted, a disconnecting caller does not cancel the attempt; only
	// the wait is bounded, and a late result is discarded.
	waitUntil := time.Now().Add(time.Duration(rec.Timeout)*time.Second + waitOverhead)
	actx, cancel := context.WithDeadline(context.WithoutCancel(ctx), waitUntil)
	defer cancel()
	out, err := d.Exec.InvokeSync(actx, SyncInvocation{
		Function:  rec,
		Revision:  revision,
		Payload:   c.Body,
		WaitUntil: waitUntil,
	})
	if refusal, ok := errors.AsType[*RefusalError](err); ok {
		if _, coded := awserrors.ResolveErrorCode(refusal.Err); coded {
			return reply{}, refusal.Err
		}
		err = refusal
	}
	if err != nil {
		slog.ErrorContext(ctx, "Lambda: synchronous attempt outcome unknown", "function_id", rec.FunctionID, "err", err)
		return reply{}, serviceError("the invocation outcome is unknown")
	}
	if out.ExecutedVersion == "" || len(out.Payload) > maxSyncPayload {
		slog.ErrorContext(ctx, "Lambda: executor returned an invalid result", "function_id", rec.FunctionID,
			"payload_bytes", len(out.Payload))
		return reply{}, serviceError("the invocation outcome is unknown")
	}
	hdr := http.Header{}
	hdr.Set("X-Amz-Executed-Version", out.ExecutedVersion)
	if out.FunctionError != "" {
		hdr.Set("X-Amz-Function-Error", out.FunctionError)
	}
	return reply{Status: http.StatusOK, Body: rawJSON(out.Payload), Header: hdr}, nil
}

// checkInvokeOptions refuses every request option this build does not
// implement rather than ignoring it.
func checkInvokeOptions(h http.Header) error {
	switch t := h.Get("X-Amz-Invocation-Type"); t {
	case "", "RequestResponse":
	case "Event", "DryRun":
		return unsupported("InvocationType " + t)
	default:
		return invalid("InvocationType must be RequestResponse, Event or DryRun")
	}
	switch l := h.Get("X-Amz-Log-Type"); l {
	case "", "None":
	case "Tail":
		return unsupported("LogType Tail")
	default:
		return invalid("LogType must be None or Tail")
	}
	for _, name := range []string{"X-Amz-Client-Context", "X-Amz-Tenant-Id", "X-Amz-Durable-Execution-Name"} {
		if _, ok := h[name]; ok {
			return unsupported(name)
		}
	}
	return nil
}
