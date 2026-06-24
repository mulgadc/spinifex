package main

import (
	"context"
	"net/http"

	"github.com/mulgadc/spinifex/cmd/ecs-agent/credentials"
	"github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/internal/ecrauth"
)

// ecrResolver implements runtime.Resolver by minting an ECR authorization token
// per pull: it fetches the host's IMDS credentials, SigV4-signs
// GetAuthorizationToken against the gateway, and returns the AWS:<jwt> as a
// user/password pair for containerd.
type ecrResolver struct {
	creds      credentials.CredentialsProvider
	region     string
	gatewayURL string
	httpClient *http.Client
}

var _ runtime.Resolver = (*ecrResolver)(nil)

func newECRResolver(creds credentials.CredentialsProvider, region, gatewayURL string, httpClient *http.Client) *ecrResolver {
	return &ecrResolver{creds: creds, region: region, gatewayURL: gatewayURL, httpClient: httpClient}
}

// Authorize returns the ECR token's user/password and proxy endpoint for ref.
func (e *ecrResolver) Authorize(ctx context.Context, ref string) (user, pass, endpoint string, err error) {
	c, err := e.creds.Retrieve(ctx)
	if err != nil {
		return "", "", "", err
	}
	tok, err := ecrauth.GetAuthorizationToken(e.region, e.gatewayURL, e.httpClient,
		c.AccessKeyID, c.SecretAccessKey, c.SessionToken)
	if err != nil {
		return "", "", "", err
	}
	return tok.Username, tok.Password, tok.ProxyHost, nil
}
