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

// CredentialsProvider yields current credentials, refreshing as needed. Retrieve
// must be safe for concurrent use.
type CredentialsProvider interface {
	Retrieve(ctx context.Context) (Credentials, error)
}
