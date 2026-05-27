package handlers_sts

import (
	"github.com/aws/aws-sdk-go/service/sts"
)

// STSService defines the interface for AWS STS operations exposed by spinifex.
//
// The implementation lives only in awsgw: STS shares the IAM at-rest envelope
// key and resolves roles via the in-process IAMService, so a daemon-side
// instance would need access to the master key — the very thing the awsgw /
// daemon trust boundary keeps separated. See docs/development/feature/sts-v1.md
// § Architecture.
type STSService interface {
	// AssumeRole mints short-lived temporary credentials bound to the target
	// role after evaluating the role's trust policy against the caller. The
	// caller identity is resolved by the gateway and passed in as plain
	// strings to keep the handler unit-testable.
	AssumeRole(callerAccountID, callerARN, callerIdentity string, input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error)

	// GetCallerIdentity returns the authenticated principal's account / ARN /
	// UserId. AWS allows every authenticated principal to call this; the
	// gateway does not gate it with checkPolicy.
	GetCallerIdentity(callerAccountID, callerARN, callerUserID string, input *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error)

	// LookupSessionCredential resolves an access-key ID to its stored
	// SessionCredential record. Used by the SigV4 middleware to verify
	// requests that carry an X-Amz-Security-Token. Returns (nil, nil) when
	// the AKID is not a session credential — neither an ASIA-prefixed AKID
	// nor a hit in the session-credentials bucket. The caller decides how to
	// translate that miss (the SigV4 verifier maps it to InvalidClientTokenId
	// on the ASIA path).
	LookupSessionCredential(accessKeyID string) (*SessionCredential, error)
}
