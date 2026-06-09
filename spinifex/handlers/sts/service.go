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

	// AssumeRoleForInstance mints role-bound temporary credentials for an EC2 instance.
	// It is the in-process IMDS entry point, NOT reachable over HTTPS: the caller is the
	// synthesised EC2 service principal (trust must allow Service ec2.amazonaws.com).
	AssumeRoleForInstance(accountID, roleARN, instanceID string, durationSeconds int64) (*sts.AssumeRoleOutput, error)

	// AssumeRoleWithWebIdentity exchanges an OIDC ID token (typically a
	// projected K8s ServiceAccount token signed by an EKS cluster's
	// per-cluster signing key) for short-lived AWS credentials bound to the
	// target IAM role. Called anonymously — the caller is identified by the
	// iss/sub/aud claims of the supplied JWT, not by SigV4.
	AssumeRoleWithWebIdentity(input *sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error)

	// GetCallerIdentity returns the authenticated principal's account / ARN /
	// UserId. AWS allows every authenticated principal to call this; the
	// gateway does not gate it with checkPolicy.
	GetCallerIdentity(callerAccountID, callerARN, callerUserID string, input *sts.GetCallerIdentityInput) (*sts.GetCallerIdentityOutput, error)

	// GetSessionToken exchanges the calling IAM user's long-lived credentials
	// for short-lived session credentials bound to the SAME user identity —
	// resolving back to arn:aws:iam::A:user/N, unlike AssumeRole. AWS forbids
	// calling it with temporary credentials: callerPrincipalType lets the
	// handler reject assumed-role callers, and callerAccessKeyID lets it reject
	// session callers — a GetSessionToken session resolves back to
	// principalType "user", so its ASIA prefix is the only signal that it is a
	// temporary credential. The gateway resolves the caller fields from the
	// SigV4 context and passes them as plain strings to keep the handler
	// testable.
	GetSessionToken(callerAccountID, callerUserName, callerPrincipalType, callerAccessKeyID string, input *sts.GetSessionTokenInput) (*sts.GetSessionTokenOutput, error)

	// VerifyPresignedGetCallerIdentity validates a SigV4-presigned URL for
	// the sts:GetCallerIdentity action — the token shape produced by `aws
	// eks get-token` — and resolves the calling principal. The EKS token
	// webhook calls this over NATS as part of TokenReview processing.
	// expectedClusterName is constant-time-compared against the signed
	// X-K8s-Aws-Id header value to prevent cross-cluster replay.
	VerifyPresignedGetCallerIdentity(presignedURL, expectedClusterName string) (*PresignedCallerIdentity, error)

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
