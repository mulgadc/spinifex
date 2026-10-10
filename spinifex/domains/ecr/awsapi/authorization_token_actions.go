package awsapi

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/ecr"
	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// AuthorizationTokenActionService composes the credential issuer and advertised
// registry endpoint used by GetAuthorizationToken. Gateway retains canonical
// IAM/STS principal construction before calling this service; ECR owns token
// mint response projection.
type AuthorizationTokenActionService struct {
	issuer   AuthorizationTokenIssuer
	endpoint RepositoryEndpoint
}

// NewAuthorizationTokenActionService binds the issuer and endpoint profile for
// GetAuthorizationToken dispatch.
func NewAuthorizationTokenActionService(issuer AuthorizationTokenIssuer, endpoint RepositoryEndpoint) *AuthorizationTokenActionService {
	return &AuthorizationTokenActionService{issuer: issuer, endpoint: endpoint}
}

var authorizationTokenActionNames = []string{"GetAuthorizationToken"}

// AuthorizationTokenActionNames returns the ECR actions served by
// AuthorizationTokenActionService. The copy keeps the coverage inventory from
// changing the service's dispatch set.
func AuthorizationTokenActionNames() []string {
	return append([]string(nil), authorizationTokenActionNames...)
}

// IsAuthorizationTokenAction reports whether action is handled by the
// composed authorization-token capability.
func IsAuthorizationTokenAction(action string) bool {
	return action == "GetAuthorizationToken"
}

// MintFor returns the AWS GetAuthorizationToken result for a canonical
// gateway-authenticated principal.
func (s *AuthorizationTokenActionService) MintFor(principal ecrauth.Principal) (*ecr.GetAuthorizationTokenOutput, error) {
	if s == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return GetAuthorizationToken(s.issuer, s.endpoint, principal)
}
