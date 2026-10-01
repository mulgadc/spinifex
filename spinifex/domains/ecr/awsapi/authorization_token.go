package awsapi

import (
	"encoding/base64"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// AuthorizationTokenIssuer is the narrow credential capability the ECR AWS
// action needs. ECR auth owns the concrete ES256 issuer; the AWS adapter owns
// its AWS JSON response projection.
type AuthorizationTokenIssuer interface {
	Mint(ecrauth.Principal) (token string, expiresAt time.Time, err error)
}

// GetAuthorizationToken mints an ECR registry token for the canonical
// SigV4-authenticated principal. The returned token is base64("AWS:<jwt>"),
// which Docker replays as Basic auth to the ECR /v2 registry endpoint.
func GetAuthorizationToken(issuer AuthorizationTokenIssuer, endpoint RepositoryEndpoint, principal ecrauth.Principal) (*ecr.GetAuthorizationTokenOutput, error) {
	if issuer == nil {
		return nil, errors.New(awserrors.ErrorNotImplemented)
	}
	token, expiresAt, err := issuer.Mint(principal)
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	authToken := base64.StdEncoding.EncodeToString([]byte("AWS:" + token))
	return &ecr.GetAuthorizationTokenOutput{
		AuthorizationData: []*ecr.AuthorizationData{{
			AuthorizationToken: aws.String(authToken),
			ProxyEndpoint:      aws.String("https://" + endpoint.RegistryURIHost(principal.AccountID)),
			ExpiresAt:          aws.Time(expiresAt),
		}},
	}, nil
}
