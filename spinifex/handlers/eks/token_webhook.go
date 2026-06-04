package handlers_eks

import (
	"encoding/base64"
	"errors"
	"strings"
)

// TokenVerifySubject is the NATS request subject the EKS token webhook calls to
// resolve an `aws eks get-token` presigned URL into a caller principal. It is
// served by the awsgw service (which hosts the in-process STS service) because
// STS is gateway-local and not otherwise exposed on NATS.
const TokenVerifySubject = "eks.VerifyToken" //nolint:gosec // NATS subject name, not a credential

// getTokenV1Prefix is the scheme prefix `aws eks get-token` prepends to the
// base64url-encoded presigned STS GetCallerIdentity URL.
const getTokenV1Prefix = "k8s-aws-v1." //nolint:gosec // token scheme prefix, not a credential

// ErrMalformedToken is returned by DecodeGetToken when the bearer token is not
// a well-formed `k8s-aws-v1.<base64url>` value.
var ErrMalformedToken = errors.New("eks: malformed k8s-aws-v1 token")

// TokenVerifyRequest is the NATS request payload sent to TokenVerifySubject.
// ClusterName is the anti-replay binding (Q10): the webhook passes its own
// cluster name and STS rejects a URL signed for any other x-k8s-aws-id.
type TokenVerifyRequest struct {
	PresignedURL string `json:"presignedURL"`
	ClusterName  string `json:"clusterName"`
}

// TokenVerifyResponse is the NATS reply: the resolved caller principal. It
// mirrors the fields of handlers_sts.PresignedCallerIdentity the webhook needs
// to key its AccessEntry lookup.
type TokenVerifyResponse struct {
	AccountID     string `json:"accountID"`
	ARN           string `json:"arn"`
	UserID        string `json:"userID"`
	PrincipalType string `json:"principalType"`
}

// DecodeGetToken extracts the presigned STS GetCallerIdentity URL from a
// `k8s-aws-v1.<base64url>` bearer token. The encoding is base64url without
// padding, matching the aws-iam-authenticator / aws-cli get-token output.
func DecodeGetToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, getTokenV1Prefix) {
		return "", ErrMalformedToken
	}
	encoded := strings.TrimPrefix(token, getTokenV1Prefix)
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", ErrMalformedToken
	}
	if len(raw) == 0 {
		return "", ErrMalformedToken
	}
	return string(raw), nil
}
