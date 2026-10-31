package gateway

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
)

// principalField names one field of the sealed principal, so a test can
// build a caller one fact at a time.
type principalField int

const (
	fieldIdentity principalField = iota
	fieldAccountID
	fieldAccessKey
	fieldPrincipalType
	fieldAssumedRoleARN
	fieldAssumedRoleID
	fieldUnderlyingRoleARN
	fieldUserID
	fieldUserARN
)

// withPrincipalField sets one field on the caller sealed in ctx, sealing a new
// caller when ctx has none. The record seals a caller once, so this rebuilds the
// context from the recorded facts when one is already sealed; it then keeps no
// other value ctx carries, which the hand-built requests it is used on do not have.
func withPrincipalField(ctx context.Context, f principalField, v string) context.Context {
	p, sealed := reqctx.Principal(ctx)
	key := reqctx.AccessKeyID(ctx)
	switch f {
	case fieldIdentity:
		p.Identity = v
	case fieldAccountID:
		p.AccountID = v
	case fieldAccessKey:
		key = v
	case fieldPrincipalType:
		p.PrincipalType = v
	case fieldAssumedRoleARN:
		p.AssumedRoleARN = v
	case fieldAssumedRoleID:
		p.AssumedRoleID = v
	case fieldUnderlyingRoleARN:
		p.UnderlyingRoleARN = v
	case fieldUserID:
		p.UserID = v
	case fieldUserARN:
		p.UserARN = v
	}
	p.CallerARN = ""
	if arn, err := credverify.CallerARN(p); err == nil {
		p.CallerARN = arn
	}
	if !sealed {
		return reqctx.WithPrincipal(ctx, p, key)
	}
	fresh := context.Background()
	if svc := reqctx.Service(ctx); svc != "" {
		fresh = reqctx.WithService(fresh, svc)
	}
	if region := reqctx.Region(ctx); region != "" {
		fresh = reqctx.WithRegion(fresh, region)
	}
	if action := reqctx.Action(ctx); action != "" {
		fresh = reqctx.WithAction(fresh, action)
	}
	return reqctx.WithPrincipal(fresh, p, key)
}
