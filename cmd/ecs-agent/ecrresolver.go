package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/mulgadc/spinifex/cmd/ecs-agent/credentials"
	"github.com/mulgadc/spinifex/cmd/ecs-agent/runtime"
	"github.com/mulgadc/spinifex/internal/ecrauth"
)

// ecrResolver implements runtime.Resolver by minting an ECR authorization token
// per pull: it fetches the host's IMDS credentials, SigV4-signs
// GetAuthorizationToken against the gateway, and returns the AWS:<jwt> as a
// user/password pair for containerd.
//
// The gateway HTTP client is built lazily on first Authorize, not at agent boot:
// a missing or malformed gateway CA only matters when an image is actually
// pulled, so the agent can still register + heartbeat on a host with absent or
// wrong ECR config.
type ecrResolver struct {
	creds      credentials.CredentialsProvider
	region     string
	gatewayURL string
	caPath     string

	mu         sync.Mutex
	httpClient *http.Client
}

var _ runtime.Resolver = (*ecrResolver)(nil)

// newECRResolver builds a resolver with an injected HTTP client (tests). The
// client is used as-is and never rebuilt from caPath.
func newECRResolver(creds credentials.CredentialsProvider, region, gatewayURL string, httpClient *http.Client) *ecrResolver {
	return &ecrResolver{creds: creds, region: region, gatewayURL: gatewayURL, httpClient: httpClient}
}

// newLazyECRResolver builds a resolver that constructs its gateway HTTP client
// from caPath on first use (production path).
func newLazyECRResolver(creds credentials.CredentialsProvider, region, gatewayURL, caPath string) *ecrResolver {
	return &ecrResolver{creds: creds, region: region, gatewayURL: gatewayURL, caPath: caPath}
}

// client returns the gateway HTTP client, building it from caPath on first call
// and caching the result. An injected client (newECRResolver) is returned as-is.
func (e *ecrResolver) client() (*http.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.httpClient != nil {
		return e.httpClient, nil
	}
	c, err := ecrauth.GatewayHTTPClient(e.caPath)
	if err != nil {
		return nil, err
	}
	e.httpClient = c
	return c, nil
}

// gatewayRegistryHost returns the host[:port] of the co-located ECR registry
// from the gateway URL; empty/unparseable yields "" (matches no ref). The
// registry shares the gateway listener, so its port is the gateway port.
func gatewayRegistryHost(gatewayURL string) string {
	u, err := url.Parse(gatewayURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// isECRRef reports whether ref targets an ECR registry: an "<acct>.dkr.ecr."
// host, or one equal to gatewayHost (the agent's own co-located registry
// advertised as a bare host/IP). Other refs pull anonymously, no token.
func isECRRef(ref, gatewayHost string) bool {
	host, _, _ := strings.Cut(ref, "/")
	if gatewayHost != "" && host == gatewayHost {
		return true
	}
	if !strings.Contains(host, ".") && !strings.Contains(host, ":") {
		return false // no registry host -> docker.io default
	}
	return strings.Contains(host, ".dkr.ecr.")
}

// Authorize returns the ECR token's user/password and proxy endpoint for ref.
// Non-ECR refs resolve to anonymous pull (empty credentials).
func (e *ecrResolver) Authorize(ctx context.Context, ref string) (user, pass, endpoint string, err error) {
	if !isECRRef(ref, gatewayRegistryHost(e.gatewayURL)) {
		return "", "", "", nil
	}
	c, err := e.creds.Retrieve(ctx)
	if err != nil {
		return "", "", "", err
	}
	httpClient, err := e.client()
	if err != nil {
		return "", "", "", err
	}
	tok, err := ecrauth.GetAuthorizationToken(e.region, e.gatewayURL, httpClient,
		c.AccessKeyID, c.SecretAccessKey, c.SessionToken)
	if err != nil {
		return "", "", "", err
	}
	return tok.Username, tok.Password, tok.ProxyHost, nil
}
