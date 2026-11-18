package ecs

import "context"

type callerARNKey struct{}

// WithCallerARN returns ctx carrying the ARN a create records as its creator:
// the IAM role ARN for CreateService, the session ARN for RegisterTaskDefinition.
func WithCallerARN(ctx context.Context, arn string) context.Context {
	return context.WithValue(ctx, callerARNKey{}, arn)
}

// CallerARN returns the ARN WithCallerARN set, or "" when none was.
func CallerARN(ctx context.Context) string {
	arn, _ := ctx.Value(callerARNKey{}).(string)
	return arn
}

// WithCaller adapts a service method to a handler that receives the caller's
// ARN from the request header, handing it on through ctx.
func WithCaller[I, O any](fn func(context.Context, *I, string) (*O, error)) func(context.Context, *I, string, string) (*O, error) {
	return func(ctx context.Context, input *I, accountID, callerARN string) (*O, error) {
		return fn(WithCallerARN(ctx, callerARN), input, accountID)
	}
}
