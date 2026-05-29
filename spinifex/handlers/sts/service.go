package handlers_sts

import (
	"github.com/aws/aws-sdk-go/service/sts"
)

// STSService defines the interface for AWS STS operations exposed by spinifex.
//
// The implementation lives only in awsgw: STS shares the IAM at-rest envelope
// key and resolves roles via the in-process IAMService, so a daemon-side
// instance would need access to the master key — the very thing the awsgw /
// daemon trust boundary keeps separated.
type STSService interface {
	// AssumeRole mints short-lived temporary credentials bound to the target
	// role after evaluating the role's trust policy against the caller. The
	// caller identity is resolved by the gateway and passed in as plain
	// strings to keep the handler unit-testable.
	AssumeRole(callerAccountID, callerARN, callerIdentity string, input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error)

	// AssumeRoleForInstance mints role-bound temporary credentials for an EC2
	// instance. It is the in-process entry point for the instance-metadata
	// service (IMDS) and is NOT reachable over HTTPS: the caller is synthesised
	// as the EC2 service principal, so the role's trust policy must allow
	// Principal: {"Service": "ec2.amazonaws.com"}, Action: "sts:AssumeRole".
	// The assumed-role session name is the instance ID. There is no way for an
	// HTTPS caller to reach this path — that separation is the security boundary
	// between user-driven AssumeRole and instance-role credential minting.
	AssumeRoleForInstance(accountID, roleARN, instanceID string, durationSeconds int64) (*sts.AssumeRoleOutput, error)

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

	// VerifySessionToken constant-time-compares the wire-form session token
	// (the X-Amz-Security-Token header) against the HMAC stored on cred.
	// Returns true on match. Encapsulating the HMAC in STSService keeps the
	// master key inside the service and off the gateway surface.
	VerifySessionToken(cred *SessionCredential, wireToken string) bool
}
