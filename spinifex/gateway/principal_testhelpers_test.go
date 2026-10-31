package gateway

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
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
// caller when ctx has none.
func withPrincipalField(ctx context.Context, f principalField, v string) context.Context {
	p, _ := reqctx.Principal(ctx)
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
	return reqctx.WithPrincipal(ctx, p, key)
}
