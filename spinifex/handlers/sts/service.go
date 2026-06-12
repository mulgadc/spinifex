package handlers_sts

import (
	"github.com/aws/aws-sdk-go/service/sts"
)

// STSService defines the interface for AWS STS operations exposed by spinifex.
// Implementation lives only in awsgw because STS shares the IAM master key,
// which must not cross the awsgw/daemon trust boundary.
type STSService interface {
	// AssumeRole mints temporary credentials after evaluating the role's trust policy.
	// Caller identity is resolved by the gateway and passed as plain strings.
	AssumeRole(callerAccountID, callerARN, callerIdentity string, input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error)

	// AssumeRoleForInstance mints instance-role credentials via the IMDS in-process path.
	// Not reachable over HTTPS; trust policy must allow Service ec2.amazonaws.com.
	AssumeRoleForInstance(accountID, roleARN, instanceID string, durationSeconds int64) (*sts.AssumeRoleOutput, error)

	// AssumeRoleWithWebIdentity exchanges an OIDC ID token for short-lived credentials
	// bound to the target role. Anonymous — caller identity comes from JWT claims, not SigV4.
	AssumeRoleWithWebIdentity(input *sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error)

	// GetCallerIdentity returns the authenticated principal's account, ARN, and UserId.
	// Allowed for every authenticated caller; not gated by checkPolicy.
	GetCallerIdentity(callerAccountID, callerARN, callerUserID string, input *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error)

	// GetSessionToken exchanges a long-lived user credential for short-lived ASIA credentials
	// that still resolve to the same user identity. Only long-lived users (not sessions) may call it.
	GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID string, input *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error)

	// VerifyPresignedGetCallerIdentity validates a SigV4-presigned GetCallerIdentity URL
	// (the `aws eks get-token` shape) and resolves the calling principal.
	// expectedClusterName is constant-time-compared against X-K8s-Aws-Id to prevent cross-cluster replay.
	VerifyPresignedGetCallerIdentity(presignedURL, expectedClusterName string) (*PresignedCallerIdentity, error)

	// LookupSessionCredential resolves an AKID to its stored SessionCredential.
	// Returns (nil, nil) when the AKID is not a session credential or has no stored record.
	LookupSessionCredential(accessKeyID string) (*SessionCredential, error)

	// VerifySessionToken constant-time-compares the wire token against the stored HMAC.
	// Keeping the HMAC inside STSService prevents the master key from crossing the gateway surface.
	VerifySessionToken(cred *SessionCredential, wireToken string) bool
}
