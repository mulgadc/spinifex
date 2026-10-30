// Package registryadapt adapts the gateway's identity, policy and host machinery to the
// registry bridge's own types, so the registry domain imports no ingress package.
package registryadapt

import (
	"errors"
	"fmt"
	"net/http"

	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	ingresshttp "github.com/mulgadc/spinifex/spinifex/ingress/http"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
)

// registryRehydrator adapts the credential rehydrator to the registry's own types.
type registryRehydrator struct{ inner credential.Rehydrator }

var _ ecrregistry.Rehydrator = registryRehydrator{}

func (a registryRehydrator) Rehydrate(accessKeyID, accountID string) (ecrregistry.Principal, error) {
	p, f := a.inner.Rehydrate(accessKeyID, accountID)
	if f != nil {
		return ecrregistry.Principal{}, registryFailure(f)
	}
	return ecrregistry.Principal{
		Identity:          p.Identity,
		AccountID:         p.AccountID,
		PrincipalType:     p.PrincipalType,
		AssumedRoleARN:    p.AssumedRoleARN,
		AssumedRoleID:     p.AssumedRoleID,
		UnderlyingRoleARN: p.UnderlyingRoleARN,
		UserID:            p.UserID,
		UserARN:           p.UserARN,
		CallerARN:         p.CallerARN,
	}, nil
}

// registryFailure maps a credential failure code to a registry sentinel: a dependency
// fault is unavailable, and every other code is an identity that no longer resolves.
func registryFailure(f *credential.Failure) error {
	if f.Code == awserrors.ErrorInternalError {
		return fmt.Errorf("%w: %s", ecrregistry.ErrUnavailable, f.Code)
	}
	return fmt.Errorf("%w: %s", ecrregistry.ErrInvalidCredential, f.Code)
}

// registryAuthorizer adapts the identity-policy evaluator to the registry's types.
type registryAuthorizer struct{ eval *policyauth.Evaluator }

var _ ecrregistry.Authorizer = registryAuthorizer{}

func (a registryAuthorizer) Authorize(r *http.Request, p ecrregistry.Principal, iamAction string, resources []string) error {
	err := a.eval.Authorize(r, credential.Principal{
		Identity:          p.Identity,
		AccountID:         p.AccountID,
		PrincipalType:     p.PrincipalType,
		AssumedRoleARN:    p.AssumedRoleARN,
		AssumedRoleID:     p.AssumedRoleID,
		UnderlyingRoleARN: p.UnderlyingRoleARN,
		UserID:            p.UserID,
		UserARN:           p.UserARN,
		CallerARN:         p.CallerARN,
	}, iamAction, resources)
	return authorizeFailure(err)
}

// authorizeFailure marks the evaluator's InternalError dependency fault as unavailable
// and passes a denial through unchanged.
func authorizeFailure(err error) error {
	if err != nil && err.Error() == awserrors.ErrorInternalError {
		return errors.Join(ecrregistry.ErrUnavailable, err)
	}
	return err
}

// registryTarget reads the account and region the host matcher recorded.
func registryTarget(r *http.Request) (account, region string, ok bool) {
	account = ingresshttp.TargetAccount(r.Context())
	return account, ingresshttp.TargetRegion(r.Context()), account != ""
}

// Config fills a BridgeConfig's identity, host and address seams from
// the gateway's credential verifier, policy evaluator and host matching.
func Config(cfg ecrregistry.BridgeConfig, creds credential.Rehydrator, eval *policyauth.Evaluator) ecrregistry.BridgeConfig {
	cfg.Rehydrator = registryRehydrator{creds}
	cfg.Authorizer = registryAuthorizer{eval}
	cfg.Target = registryTarget
	cfg.ClientIP = ingresshttp.RequestClientIP
	return cfg
}
