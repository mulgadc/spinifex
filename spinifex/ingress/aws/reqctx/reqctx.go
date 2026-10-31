// Package reqctx carries the request-scoped facts the AWS ingress pipeline
// establishes: the claimed signing service, the credential-scope Region, the
// resolved action label and the verified caller. Every fact lives in one record
// under one context key, and each With* returns a derived context so a reader
// upstream of a writer never sees the write. Only the gateway and packages under
// ingress/aws may import it, so a dispatcher still receives only dispatch.Invocation.
package reqctx

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
)

type key struct{}

// record is immutable once stored: a With* copies it.
type record struct {
	service      string
	region       string
	action       string
	accessKeyID  string
	principal    credential.Principal
	hasPrincipal bool
}

func load(ctx context.Context) record {
	r, _ := ctx.Value(key{}).(record)
	return r
}

func store(ctx context.Context, r record) context.Context {
	return context.WithValue(ctx, key{}, r)
}

// WithService records the credential-scope service the request claims.
func WithService(ctx context.Context, service string) context.Context {
	r := load(ctx)
	r.service = service
	return store(ctx, r)
}

// Service is the credential-scope service, or "" before one is recorded.
func Service(ctx context.Context) string { return load(ctx).service }

// WithRegion records the credential-scope Region.
func WithRegion(ctx context.Context, region string) context.Context {
	r := load(ctx)
	r.region = region
	return store(ctx, r)
}

// Region is the credential-scope Region, or "" before one is recorded.
func Region(ctx context.Context) string { return load(ctx).region }

// WithAction records the resolved action label. A registration's resolver may
// refine it before dispatch.
func WithAction(ctx context.Context, action string) context.Context {
	r := load(ctx)
	r.action = action
	return store(ctx, r)
}

// Action is the resolved action label, or "" when none is known.
func Action(ctx context.Context) string { return load(ctx).action }

// WithPrincipal seals the verified caller and the access key id it presented.
// The SigV4 stage calls it once, after Resolve succeeds.
func WithPrincipal(ctx context.Context, p credential.Principal, accessKeyID string) context.Context {
	r := load(ctx)
	r.principal = p
	r.accessKeyID = accessKeyID
	r.hasPrincipal = true
	return store(ctx, r)
}

// Principal is the verified caller; ok is false when the request was not authenticated.
func Principal(ctx context.Context) (p credential.Principal, ok bool) {
	r := load(ctx)
	return r.principal, r.hasPrincipal
}

// AccessKeyID is the access key id the verified caller presented.
func AccessKeyID(ctx context.Context) string { return load(ctx).accessKeyID }
