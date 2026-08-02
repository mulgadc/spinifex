// Package imdscreds fetches AWS instance-role credentials from the in-guest
// IMDS endpoint (169.254.169.254) using IMDSv2. It is shared by the binaries
// that run inside a Spinifex guest and need the node/instance role credentials:
// the EKS ecr-credential-provider and the ECS ecs-agent.
//
// It wraps the AWS SDK's feature/ec2/imds client and credentials/ec2rolecreds
// provider rather than hand-rolling PUT-token/GET requests, so it inherits the
// SDK's Code-field validation, IMDSv1 fallback policy and strict RFC3339
// expiry parsing (a malformed Expiration is a hard error, not "never expires").
package imdscreds

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
)

// Credentials is a resolved instance-role credential set with parsed expiry.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// Fetch resolves the instance role then its credentials via the SDK's IMDSv2
// client and the ec2rolecreds provider. base is the IMDS root, e.g.
// http://169.254.169.254/latest.
func Fetch(client *http.Client, base string) (Credentials, error) {
	creds, err := NewProvider(client, base).Retrieve(context.Background())
	if err != nil {
		return Credentials{}, fmt.Errorf("fetch IMDS credentials: %w", err)
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return Credentials{}, fmt.Errorf("IMDS credentials missing access/secret key")
	}
	return Credentials{
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		SessionToken:    creds.SessionToken,
		Expiration:      creds.Expires,
	}, nil
}

// NewProvider builds an ec2rolecreds provider against base, an IMDS root URL
// such as http://169.254.169.254/latest. A nil client uses the SDK IMDS
// client's default HTTP client.
func NewProvider(client *http.Client, base string) aws.CredentialsProvider {
	imdsClient := NewClient(client, base)
	return ec2rolecreds.New(func(o *ec2rolecreds.Options) {
		o.Client = imdsClient
	})
}

// NewClient builds an SDK IMDS client against base, an IMDS root URL such as
// http://169.254.169.254/latest. A trailing /latest is stripped since the
// client appends its own; both forms of base keep working. A nil client uses
// the SDK IMDS client's default HTTP client.
func NewClient(client *http.Client, base string) *imds.Client {
	opts := imds.Options{Endpoint: rootEndpoint(base)}
	if client != nil {
		opts.HTTPClient = client
	}
	return imds.New(opts)
}

// rootEndpoint strips a trailing /latest from base so it can be used as the
// SDK IMDS client's Endpoint, which is the scheme+host root — the client
// prepends its own /latest to every request path.
func rootEndpoint(base string) string {
	base = strings.TrimRight(base, "/")
	return strings.TrimSuffix(base, "/latest")
}
