// Package gwsign SigV4-signs on-VM requests to the AWS gateway. Credentials
// come from either fixed long-lived keys or the AWS SDK default credential
// chain, which resolves the in-VM IMDS instance-role credentials and rotates
// them automatically. A session token, when present, is signed as
// X-Amz-Security-Token so the gateway can verify temporary (ASIA) credentials.
package gwsign

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// Signer holds a credential provider and signs requests in place.
type Signer struct {
	provider aws.CredentialsProvider
}

// NewStatic returns a Signer using fixed long-lived keys (no session token).
func NewStatic(accessKey, secretKey string) *Signer {
	return &Signer{provider: credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")}
}

// NewIMDS returns a Signer backed by the AWS SDK default credential chain. In a
// guest VM with no env/profile credentials the chain falls back to the IMDS
// endpoint (169.254.169.254), yielding scoped, auto-rotating instance-role
// credentials. region sets the chain's default region.
func NewIMDS(ctx context.Context, region string) (*Signer, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS default config: %w", err)
	}
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("default credential chain resolved no provider")
	}
	return &Signer{provider: cfg.Credentials}, nil
}

// EnsureCredentials retrieves once from the provider, surfacing the error so a
// caller can warm the IMDS instance-role datapath before the first Sign. The
// SDK's IMDS client fast-fails each probe (250ms dial), so a cold per-tap
// datapath returns "context deadline exceeded" / "no EC2 IMDS role found" here;
// a caller loops until this succeeds. A static signer always succeeds.
func (s *Signer) EnsureCredentials(ctx context.Context) error {
	if _, err := s.provider.Retrieve(ctx); err != nil {
		return fmt.Errorf("retrieve credentials: %w", err)
	}
	return nil
}

// Sign signs r in place for service/region. payloadHash is the hex SHA-256 of
// the body (or a SigV4 sentinel). Credentials are retrieved per call so rotated
// IMDS credentials take effect without a restart; the SDK signer sets
// X-Amz-Security-Token when the credentials carry a session token.
func (s *Signer) Sign(r *http.Request, payloadHash, service, region string) error {
	creds, err := s.provider.Retrieve(r.Context())
	if err != nil {
		return fmt.Errorf("retrieve credentials: %w", err)
	}
	// The gateway recovers the payload hash from this header for non-S3
	// services; the SDK signer alone does not set it for direct callers.
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	return v4.NewSigner().SignHTTP(r.Context(), creds, r, payloadHash, service, region, time.Now().UTC())
}
