package credentials

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/internal/imdscreds"
)

// refreshMargin is how long before expiry the cache is considered stale.
const refreshMargin = 5 * time.Minute

// IMDSProvider fetches instance-role credentials from IMDS and caches them until
// they near expiry. Safe for concurrent use.
type IMDSProvider struct {
	client *http.Client
	base   string

	mu     sync.Mutex
	cached Credentials
}

var _ CredentialsProvider = (*IMDSProvider)(nil)

// NewIMDSProvider builds a provider against the given IMDS base URL (e.g.
// "http://169.254.169.254/latest"). A nil client uses http.DefaultClient.
func NewIMDSProvider(client *http.Client, base string) *IMDSProvider {
	if client == nil {
		client = http.DefaultClient
	}
	return &IMDSProvider{client: client, base: base}
}

// Retrieve returns cached credentials when still valid, else fetches fresh ones.
func (p *IMDSProvider) Retrieve(ctx context.Context) (Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cached.Valid(refreshMargin) {
		return p.cached, nil
	}
	if err := ctx.Err(); err != nil {
		return Credentials{}, err
	}

	raw, err := imdscreds.Fetch(p.client, p.base)
	if err != nil {
		return Credentials{}, err
	}
	p.cached = Credentials{
		AccessKeyID:     raw.AccessKeyID,
		SecretAccessKey: raw.SecretAccessKey,
		SessionToken:    raw.SessionToken,
		Expiration:      raw.Expiration,
	}
	return p.cached, nil
}
