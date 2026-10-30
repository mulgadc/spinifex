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
	return projectPrincipal(p), nil
}

func projectPrincipal(p credential.Principal) ecrregistry.Principal {
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
	}
}

// registryRoleRehydrator adapts the credential role rehydrator to the registry's types.
type registryRoleRehydrator struct{ inner credential.RoleRehydrator }

var _ ecrregistry.RoleRehydrator = registryRoleRehydrator{}

func (a registryRoleRehydrator) RehydrateRole(accountID, roleID, assumedRoleARN string) (ecrregistry.Principal, error) {
	p, f := a.inner.RehydrateRole(accountID, roleID, assumedRoleARN)
	if f != nil {
		if f.RoleGone {
			return ecrregistry.Principal{}, fmt.Errorf("%w: %s", ecrregistry.ErrRoleGone, f.Code)
		}
		return ecrregistry.Principal{}, registryFailure(f)
	}
	return projectPrincipal(p), nil
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

// Credentials is what the bridge needs of the credential verifier: the caller an
// access key names, and the caller a role names once its session is gone.
type Credentials interface {
	credential.Rehydrator
	credential.RoleRehydrator
}

// Config fills a BridgeConfig's identity, host and address seams from
// the gateway's credential verifier, policy evaluator and host matching.
func Config(cfg ecrregistry.BridgeConfig, creds Credentials, eval *policyauth.Evaluator) ecrregistry.BridgeConfig {
	cfg.Rehydrator = registryRehydrator{creds}
	cfg.RoleRehydrator = registryRoleRehydrator{creds}
	cfg.Authorizer = registryAuthorizer{eval}
	cfg.Target = registryTarget
	cfg.ClientIP = ingresshttp.RequestClientIP
	return cfg
}
