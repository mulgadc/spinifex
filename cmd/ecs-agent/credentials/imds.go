package credentials

import (
	"context"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/mulgadc/spinifex/internal/imdscreds"
)

// IMDSProvider fetches instance-role credentials from IMDS via the AWS SDK's
// EC2 IMDS client and ec2rolecreds provider, cached with aws.CredentialsCache
// (which refetches once the real expiry passes; ec2rolecreds also caps
// Expires to at most an hour out). Safe for concurrent use.
type IMDSProvider struct {
	cache *aws.CredentialsCache
}

var _ CredentialsProvider = (*IMDSProvider)(nil)

// NewIMDSProvider builds a provider against the given IMDS base URL (e.g.
// "http://169.254.169.254/latest"). A nil client uses the SDK IMDS client's
// default HTTP client.
func NewIMDSProvider(client *http.Client, base string) *IMDSProvider {
	provider := imdscreds.NewProvider(client, base)
	return &IMDSProvider{cache: aws.NewCredentialsCache(provider)}
}

// Retrieve returns cached credentials when still valid, else fetches fresh ones.
func (p *IMDSProvider) Retrieve(ctx context.Context) (Credentials, error) {
	creds, err := p.cache.Retrieve(ctx)
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
		Expiration:      creds.Expires,
	}, nil
}
