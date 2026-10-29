package gateway

import "context"

// principalField names one field of the sealed callerPrincipal, so a test can
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
	p, _ := callerPrincipalFrom(ctx)
	switch f {
	case fieldIdentity:
		p.identity = v
	case fieldAccountID:
		p.accountID = v
	case fieldAccessKey:
		p.accessKeyID = v
	case fieldPrincipalType:
		p.principalType = v
	case fieldAssumedRoleARN:
		p.assumedRoleARN = v
	case fieldAssumedRoleID:
		p.assumedRoleID = v
	case fieldUnderlyingRoleARN:
		p.underlyingRoleARN = v
	case fieldUserID:
		p.userID = v
	case fieldUserARN:
		p.userARN = v
	}
	return withCallerPrincipal(ctx, p)
}
