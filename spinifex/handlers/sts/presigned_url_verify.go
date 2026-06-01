package handlers_sts

import (
	"errors"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// PresignedCallerIdentity is the result of validating a SigV4-presigned
// `sts:GetCallerIdentity` URL — the token shape produced by `aws eks
// get-token` and consumed by the EKS token webhook (`eks-token-webhook`).
//
// The full implementation lands in a follow-up commit on this branch:
// SigV4 canonical-request reconstruction, signature verification against
// the named access key's secret, expiry check, and the mandatory
// `X-K8s-Aws-Id` header equality check (per eks-v1.md Q10) that prevents a
// token minted for cluster-A from being replayed against cluster-B.
type PresignedCallerIdentity struct {
	AccountID     string
	ARN           string
	UserID        string
	XK8sAwsID     string // value the caller signed under, for the webhook to verify
	PrincipalType string
}

// VerifyPresignedGetCallerIdentity validates a SigV4-presigned URL for the
// `sts:GetCallerIdentity` action and resolves the calling principal. The EKS
// token webhook calls this over NATS as part of TokenReview processing.
//
// expectedClusterName is the cluster the webhook lives on; the function
// constant-time-compares the `X-K8s-Aws-Id` signed-header value against it
// to prevent cross-cluster replay (eks-v1.md Q10 mandatory check).
func (s *STSServiceImpl) VerifyPresignedGetCallerIdentity(presignedURL, expectedClusterName string) (*PresignedCallerIdentity, error) {
	if presignedURL == "" || expectedClusterName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	return nil, errors.New(awserrors.ErrorNotImplemented)
}
