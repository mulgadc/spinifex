package handlers_iam

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Per-account IAM JetStream KV bucket holds resources that are inherently
// account-scoped and not appropriate for the global IAM buckets — currently
// the OIDC identity-provider registry consumed by STS AssumeRoleWithWebIdentity
// and (in a later sprint) by IAM's CreateOpenIDConnectProvider CRUD.
//
// Lazy creation: callers that read MUST tolerate ErrBucketNotFound (no provider
// has been registered for the account yet); callers that write MUST use
// GetOrCreateIAMAccountBucket so the bucket appears on first use.
const (
	KVBucketIAMAccountPrefix  = "iam-account-"
	KVBucketIAMAccountVersion = 1

	// oidcProvidersKeyPrefix matches the eks-v1.md Q4 layout:
	// "iam-account-{accountID}/oidc-providers/{issuerHash}". Keep the literal
	// here (not strings.Join) so a stray refactor that introduces a different
	// separator fails compile rather than silently writing to a sibling path.
	oidcProvidersKeyPrefix = "oidc-providers/"
)

// IAMAccountBucketName returns the JetStream KV bucket name for the supplied
// AWS account ID.
func IAMAccountBucketName(accountID string) string {
	return KVBucketIAMAccountPrefix + accountID
}

// OIDCProviderKey returns the KV key for an OIDC provider entry. The key is
// derived from the SHA-256 hex digest of the issuer URL — the Mulga
// convention (AWS uses the SHA-1 thumbprint of the IdP root cert; we hash the
// issuer URL because we are also the IdP and have no separate cert chain to
// pin against).
func OIDCProviderKey(issuer string) string {
	sum := sha256.Sum256([]byte(issuer))
	return oidcProvidersKeyPrefix + hex.EncodeToString(sum[:])
}

// OIDCProviderARN composes the AWS-format ARN used as the Federated principal
// value in role trust policies that authorise the named issuer:
//
//	arn:aws:iam::{accountID}:oidc-provider/{issuerHostPath}
//
// issuerHostPath is the issuer URL with the scheme stripped (no leading
// "https://") — matches the AWS convention so customers can paste an existing
// IRSA trust policy unchanged.
func OIDCProviderARN(accountID, issuerHostPath string) string {
	return fmt.Sprintf("arn:aws:iam::%s:oidc-provider/%s", accountID, issuerHostPath)
}
