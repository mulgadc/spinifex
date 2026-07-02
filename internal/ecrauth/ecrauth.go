// Package ecrauth mints ECR registry credentials by SigV4-signing
// GetAuthorizationToken against the Spinifex AWS gateway's ECR endpoint and
// decoding the returned AWS:<jwt> token into Basic-auth parts. It is shared by
// the in-guest binaries that pull images from the internal registry: the EKS
// ecr-credential-provider and the ECS ecs-agent.
package ecrauth

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/internal/tlsconfig"
)

// defaultRegion is used when the caller supplies no region; SigV4 still needs a
// scope and the gateway does not enforce a specific value.
const defaultRegion = "us-east-1"

// Token is the decoded result of GetAuthorizationToken: Basic-auth parts plus
// the registry proxy host and token expiry.
type Token struct {
	Username  string
	Password  string
	ProxyHost string
	ExpiresAt time.Time
}

// GetAuthorizationToken builds an aws-sdk ECR client pointed at the gateway and
// calls GetAuthorizationToken with the supplied static credentials, decoding the
// AWS:<jwt> token into Basic-auth parts. httpClient must trust the gateway CA.
func GetAuthorizationToken(region, gatewayURL string, httpClient *http.Client, akid, secret, sessionToken string) (Token, error) {
	if region == "" {
		region = defaultRegion
	}
	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String(region),
		Endpoint:    aws.String(gatewayURL),
		Credentials: credentials.NewStaticCredentials(akid, secret, sessionToken),
		HTTPClient:  httpClient,
		DisableSSL:  aws.Bool(false),
	})
	if err != nil {
		return Token{}, fmt.Errorf("new session: %w", err)
	}

	out, err := ecr.New(sess).GetAuthorizationToken(&ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return Token{}, fmt.Errorf("GetAuthorizationToken: %w", err)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return Token{}, fmt.Errorf("GetAuthorizationToken returned no authorization data")
	}
	data := out.AuthorizationData[0]

	raw, err := base64.StdEncoding.DecodeString(aws.StringValue(data.AuthorizationToken))
	if err != nil {
		return Token{}, fmt.Errorf("decode authorization token: %w", err)
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return Token{}, fmt.Errorf("authorization token not in user:password form")
	}

	tok := Token{
		Username:  user,
		Password:  pass,
		ProxyHost: HostFromImage(aws.StringValue(data.ProxyEndpoint)),
	}
	if data.ExpiresAt != nil {
		tok.ExpiresAt = aws.TimeValue(data.ExpiresAt)
	}
	return tok, nil
}

// GatewayHTTPClient builds an HTTP client trusting the gateway CA at caPath.
// When caPath is empty it relies on the system trust store.
func GatewayHTTPClient(caPath string) (*http.Client, error) {
	tlsCfg := &tls.Config{
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: tlsconfig.Curves,
	}
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read gateway CA %q: %w", caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("gateway CA %q has no usable certificates", caPath)
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second},
	}, nil
}

// HostFromImage returns the host[:port] portion of an image ref or URL. It strips
// any scheme and trims at the first path separator.
func HostFromImage(image string) string {
	if i := strings.Index(image, "://"); i >= 0 {
		image = image[i+3:]
	}
	if i := strings.IndexByte(image, '/'); i >= 0 {
		image = image[:i]
	}
	return image
}
