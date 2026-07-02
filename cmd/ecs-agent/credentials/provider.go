// Package credentials supplies AWS SigV4 credentials to the ecs-agent. The agent
// signs gateway calls (ECR GetAuthorizationToken, instance register/heartbeat)
// with the host's instance-role credentials, fetched from IMDS and cached until
// near expiry.
package credentials

import (
	"context"
	"time"
)

// Credentials is a SigV4 credential set with an expiry.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// Valid reports whether the credentials are usable now, leaving a margin so a
// caller does not start a request with a token about to expire mid-flight.
func (c Credentials) Valid(margin time.Duration) bool {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return false
	}
	if c.Expiration.IsZero() {
		return true
	}
	return time.Now().Add(margin).Before(c.Expiration)
}

// CredentialsProvider yields current credentials, refreshing as needed. Retrieve
// must be safe for concurrent use.
type CredentialsProvider interface {
	Retrieve(ctx context.Context) (Credentials, error)
}
