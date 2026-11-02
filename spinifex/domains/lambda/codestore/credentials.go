package codestore

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/service/sts"
	telemetry "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
)

const (
	// refreshMargin re-assumes this long before a session expires.
	refreshMargin = 5 * time.Minute
	// retryInterval spaces re-assume attempts while an old session is still valid.
	retryInterval = 30 * time.Second
)

// AssumeFunc mints a code-store session. The composition layer binds it to the
// in-process STS entry point for one Region.
type AssumeFunc func() (*sts.AssumeRoleOutput, error)

// sessionProvider re-assumes the role ahead of expiry. A failed re-assume keeps
// the current session in use until it expires, then fails closed.
type sessionProvider struct {
	assume AssumeFunc
	now    func() time.Time

	mu        sync.Mutex
	value     credentials.Value
	expires   time.Time
	lastError time.Time
	// minRemaining makes the next Retrieve re-assume unless the session has at
	// least this long left; it is cleared by that Retrieve.
	minRemaining time.Duration
}

var (
	_ credentials.Provider = (*sessionProvider)(nil)
	_ credentials.Expirer  = (*sessionProvider)(nil)
)

func newSessionProvider(assume AssumeFunc, now func() time.Time) *sessionProvider {
	return &sessionProvider{assume: assume, now: now}
}

func (p *sessionProvider) Retrieve() (credentials.Value, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	valid := p.value.AccessKeyID != "" && now.Before(p.expires)
	need := p.minRemaining
	p.minRemaining = 0
	if need > 0 {
		if valid && p.expires.Sub(now) >= need {
			return p.value, nil
		}
	} else if valid && (now.Before(p.expires.Add(-refreshMargin)) || now.Before(p.lastError.Add(retryInterval))) {
		return p.value, nil
	}

	out, err := p.assume()
	if err == nil && (out == nil || out.Credentials == nil) {
		err = errors.New("empty session")
	}
	if err != nil {
		p.lastError = now
		if valid {
			slog.Warn("Lambda code store: re-assume failed; keeping current session",
				"remaining_ms", telemetry.Millis(p.expires.Sub(now)), "err", err)
			return p.value, nil
		}
		return credentials.Value{}, fmt.Errorf("assume code-store role: %w", err)
	}

	c := out.Credentials
	p.value = credentials.Value{
		AccessKeyID:     aws.StringValue(c.AccessKeyId),
		SecretAccessKey: aws.StringValue(c.SecretAccessKey),
		SessionToken:    aws.StringValue(c.SessionToken),
		ProviderName:    "LambdaCodeStoreSession",
	}
	p.expires = aws.TimeValue(c.Expiration)
	p.lastError = time.Time{}
	return p.value, nil
}

// IsExpired reports true inside the refresh margin so the SDK calls Retrieve.
func (p *sessionProvider) IsExpired() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.value.AccessKeyID == "" || !p.now().Before(p.expires.Add(-refreshMargin))
}

// ExpiresAt is when the current session stops working.
func (p *sessionProvider) ExpiresAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.expires
}

// requireRemaining makes the next Retrieve draw a new session unless the
// current one outlasts d.
func (p *sessionProvider) requireRemaining(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.minRemaining = d
}
