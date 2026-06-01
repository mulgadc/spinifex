package handlers_sts

import (
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// AssumeRoleWithWebIdentity exchanges an OIDC ID token (typically a
// projected Kubernetes ServiceAccount token signed by an EKS cluster's
// per-cluster signing key) for short-lived AWS credentials bound to the
// target IAM role. The full v1 implementation lands in a follow-up commit
// on this branch: JWT signature verification against the cluster JWKS
// (FetchClusterJWKS), `aud` / `exp` / `iss` claim validation, OIDC provider
// registration check, and Federated-principal trust-policy evaluation.
//
// The interface entry, gateway dispatch, and oidc_jwks.go reader land in
// this checkpoint so downstream sprints can call the method against a
// stubbed handler while the verify path is being written.
func (s *STSServiceImpl) AssumeRoleWithWebIdentity(input *sts.AssumeRoleWithWebIdentityInput) (*sts.AssumeRoleWithWebIdentityOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	slog.Debug("AssumeRoleWithWebIdentity invoked (stub)",
		"role_arn", deref(input.RoleArn),
		"session_name", deref(input.RoleSessionName),
		"provider_id", deref(input.ProviderId),
	)
	return nil, errors.New(awserrors.ErrorNotImplemented)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
