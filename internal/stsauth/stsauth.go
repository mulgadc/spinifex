// Package stsauth mints task-role credentials by SigV4-signing sts:AssumeRole
// against the Spinifex AWS gateway. It mirrors internal/ecrauth: the in-guest
// ecs-agent already holds instance-role credentials and signs gateway calls with
// them, so it assumes a task's IAM role the same way rather than reaching STS
// over NATS (the agent has no NATS connection).
package stsauth

import (
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	awscreds "github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
)

// defaultRegion is used when the caller supplies no region; SigV4 still needs a
// scope and the gateway does not enforce a specific value.
const defaultRegion = "us-east-1"

// Credentials is the decoded result of AssumeRole: a SigV4 credential set bound
// to the assumed role, with its expiry.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// AssumeRole builds an aws-sdk STS client pointed at the gateway and calls
// AssumeRole with the supplied static (instance-role) credentials, returning the
// assumed-role credential set. httpClient must trust the gateway CA.
func AssumeRole(region, gatewayURL string, httpClient *http.Client, akid, secret, sessionToken, roleARN, sessionName string) (Credentials, error) {
	if region == "" {
		region = defaultRegion
	}
	if roleARN == "" || sessionName == "" {
		return Credentials{}, fmt.Errorf("stsauth: roleARN and sessionName are required")
	}
	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String(region),
		Endpoint:    aws.String(gatewayURL),
		Credentials: awscreds.NewStaticCredentials(akid, secret, sessionToken),
		HTTPClient:  httpClient,
		DisableSSL:  aws.Bool(false),
	})
	if err != nil {
		return Credentials{}, fmt.Errorf("new session: %w", err)
	}

	out, err := sts.New(sess).AssumeRole(&sts.AssumeRoleInput{
		RoleArn:         aws.String(roleARN),
		RoleSessionName: aws.String(sessionName),
	})
	if err != nil {
		return Credentials{}, fmt.Errorf("AssumeRole: %w", err)
	}
	if out.Credentials == nil {
		return Credentials{}, fmt.Errorf("AssumeRole returned no credentials")
	}
	c := out.Credentials
	return Credentials{
		AccessKeyID:     aws.StringValue(c.AccessKeyId),
		SecretAccessKey: aws.StringValue(c.SecretAccessKey),
		SessionToken:    aws.StringValue(c.SessionToken),
		Expiration:      aws.TimeValue(c.Expiration),
	}, nil
}
