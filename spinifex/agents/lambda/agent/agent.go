// Package agent runs the guest loop: take a request from the host link, hand
// it to the function runtime, and report the outcome back.
package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
)

const (
	errTypeTimedOut = "Sandbox.Timedout"
	errTypeUnknown  = "Runtime.Unknown"
)

// Run serves requests from link until the host asks for shutdown (nil), the
// runtime reports an init error, ctx ends, or the link fails. GrantID and
// AttemptID are echoed to the host unchanged.
func Run(ctx context.Context, link hostlink.Link, srv *runtimeapi.Server) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		select {
		case <-srv.InitFailed():
			ie, _ := srv.InitError()
			cancel(fmt.Errorf("runtime init error %q: %s", ie.Type, ie.Body))
		case <-ctx.Done():
		}
	}()

	for {
		req, err := link.Next(ctx)
		if errors.Is(err, hostlink.ErrShutdown) {
			return nil
		}
		if err != nil {
			return cause(ctx, err)
		}

		res, err := srv.Invoke(ctx, runtimeapi.Invocation{
			RequestID:   req.RequestID,
			FunctionARN: req.FunctionARN,
			Deadline:    time.UnixMilli(req.DeadlineMs),
			TraceID:     req.TraceID,
			Payload:     req.Payload,
		})
		if err != nil {
			if ctx.Err() != nil {
				return cause(ctx, err)
			}
			res = failure(err)
		}
		resp := hostlink.Response{
			GrantID:       req.GrantID,
			AttemptID:     req.AttemptID,
			RequestID:     req.RequestID,
			Payload:       res.Payload,
			FunctionError: res.FunctionError,
		}
		if err := link.Complete(ctx, resp); err != nil {
			return cause(ctx, err)
		}
	}
}

func cause(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return err
}

func failure(err error) runtimeapi.Result {
	errType := errTypeUnknown
	if errors.Is(err, runtimeapi.ErrDeadlineExceeded) {
		errType = errTypeTimedOut
	}
	return runtimeapi.Result{Payload: runtimeapi.ErrorBody(err.Error(), errType), FunctionError: errType}
}
