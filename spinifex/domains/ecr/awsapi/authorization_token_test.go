package awsapi

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAuthorizationTokenIssuer struct {
	got       ecrauth.Principal
	token     string
	expiresAt time.Time
	err       error
}

func (i *fakeAuthorizationTokenIssuer) Mint(principal ecrauth.Principal) (string, time.Time, error) {
	i.got = principal
	return i.token, i.expiresAt, i.err
}

func TestGetAuthorizationToken_ProjectsMintedCredential(t *testing.T) {
	expiresAt := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	issuer := &fakeAuthorizationTokenIssuer{token: "signed-token", expiresAt: expiresAt}
	principal := ecrauth.Principal{
		AccountID: "123456789012", ARN: "arn:aws:iam::123456789012:user/dev",
		Type: "user", AccessKeyID: "AKIAEXAMPLE",
	}

	out, err := GetAuthorizationToken(issuer, RepositoryEndpoint{
		Region: "ap-southeast-2", ServicesDomain: "spinifex.test", RegistryPort: "9999",
	}, principal)
	require.NoError(t, err)
	require.Len(t, out.AuthorizationData, 1)
	data := out.AuthorizationData[0]
	assert.Equal(t, principal, issuer.got)
	assert.Equal(t, "https://123456789012.dkr.ecr.ap-southeast-2.spinifex.test:9999", *data.ProxyEndpoint)
	assert.Equal(t, expiresAt, *data.ExpiresAt)
	decoded, err := base64.StdEncoding.DecodeString(*data.AuthorizationToken)
	require.NoError(t, err)
	assert.Equal(t, "AWS:signed-token", string(decoded))
}

func TestGetAuthorizationToken_RefusesUnavailableIssuer(t *testing.T) {
	principal := ecrauth.Principal{AccountID: "123456789012"}
	_, err := GetAuthorizationToken(nil, RepositoryEndpoint{}, principal)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, awserrors.ValidErrorCodeFromError(err))

	_, err = GetAuthorizationToken(&fakeAuthorizationTokenIssuer{err: errors.New("signer unavailable")}, RepositoryEndpoint{}, principal)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, awserrors.ValidErrorCodeFromError(err))
}
