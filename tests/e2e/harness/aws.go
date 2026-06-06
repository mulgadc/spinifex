//go:build e2e

package harness

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
)

// AWSClient bundles the SDK service handles a scenario typically needs against
// the local Spinifex AWS gateway. Region is fixed to ap-southeast-2 to match
// the bash scripts; override via SPINIFEX_AWS_REGION.
type AWSClient struct {
	EC2   *ec2.EC2
	ELBv2 *elbv2.ELBV2
	EKS   *eks.EKS
	IAM   *iam.IAM
	STS   *sts.STS
}

// NewAWSClient builds clients pointed at https://<endpoint>:<port>/ using the
// spinifex CA for TLS verification. Credentials come from the AWS_PROFILE env
// var (matching the bash scripts) — default `spinifex`. Override via
// SPINIFEX_AWS_ACCESS_KEY_ID / SPINIFEX_AWS_SECRET_ACCESS_KEY for static creds.
func NewAWSClient(t *testing.T, env *Env) *AWSClient {
	t.Helper()
	id, secret := os.Getenv("SPINIFEX_AWS_ACCESS_KEY_ID"), os.Getenv("SPINIFEX_AWS_SECRET_ACCESS_KEY")
	return newAWSClient(t, env, id, secret, "")
}

// NewAWSClientWithCreds builds an AWSClient with explicit static credentials
// — used by AccountCarousel to scope a client to a tenant account created
// via `spx admin account create`. Bypasses AWS_PROFILE shared-config lookup.
func NewAWSClientWithCreds(t *testing.T, env *Env, accessKey, secretKey string) *AWSClient {
	t.Helper()
	if accessKey == "" || secretKey == "" {
		t.Fatalf("NewAWSClientWithCreds: empty credentials")
	}
	return newAWSClient(t, env, accessKey, secretKey, "")
}

// NewAWSClientWithSessionCreds builds an AWSClient with static temporary
// credentials issued by sts:AssumeRole. The SDK signs every request with the
// supplied session token in X-Amz-Security-Token, driving the gateway's ASIA
// auth path.
func NewAWSClientWithSessionCreds(t *testing.T, env *Env, accessKey, secretKey, sessionToken string) *AWSClient {
	t.Helper()
	if accessKey == "" || secretKey == "" || sessionToken == "" {
		t.Fatalf("NewAWSClientWithSessionCreds: empty credentials")
	}
	return newAWSClient(t, env, accessKey, secretKey, sessionToken)
}

func newAWSClient(t *testing.T, env *Env, accessKey, secretKey, sessionToken string) *AWSClient {
	t.Helper()

	endpoint := os.Getenv("SPINIFEX_AWS_ENDPOINT")
	if endpoint == "" {
		host := "127.0.0.1"
		if len(env.ServiceIPs) > 0 {
			host = env.ServiceIPs[0]
		}
		endpoint = fmt.Sprintf("https://%s:%d", host, env.AWSGWPort)
	}
	region := getenv("SPINIFEX_AWS_REGION", "ap-southeast-2")

	// Runner-resident scenarios (e.g. reboot suite running outside the VM)
	// don't have the spinifex CA cert on disk. SPINIFEX_AWS_INSECURE=1 skips
	// the CA load entirely and uses InsecureSkipVerify — trust validation is
	// already covered by the cert suite, so this is safe for non-cert tests.
	tlsCfg := &tls.Config{}
	if os.Getenv("SPINIFEX_AWS_INSECURE") == "1" {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // explicit opt-in for runner-resident E2E
	} else {
		caPath, err := ResolveCACert(env)
		if err != nil {
			t.Fatalf("AWS client: %v", err)
		}
		pool, err := LoadCAPool(caPath)
		if err != nil {
			t.Fatalf("AWS client: %v", err)
		}
		tlsCfg.RootCAs = pool
	}

	cfg := &aws.Config{
		Endpoint:         aws.String(endpoint),
		Region:           aws.String(region),
		S3ForcePathStyle: aws.Bool(true),
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: tlsCfg,
			},
		},
	}

	opts := session.Options{Config: *cfg}
	if accessKey != "" && secretKey != "" {
		// Static creds bypass shared-config lookup — required for the
		// per-tenant carousel where each Profile holds its own access key,
		// and for STS-issued session credentials (sessionToken non-empty).
		cfg.Credentials = credentials.NewStaticCredentials(accessKey, secretKey, sessionToken)
		opts.Config = *cfg
	} else {
		opts.SharedConfigState = session.SharedConfigEnable
		opts.Profile = getenv("AWS_PROFILE", "spinifex")
	}

	sess, err := session.NewSessionWithOptions(opts)
	if err != nil {
		t.Fatalf("AWS session: %v", err)
	}

	return &AWSClient{
		EC2:   ec2.New(sess),
		ELBv2: elbv2.New(sess),
		EKS:   eks.New(sess),
		IAM:   iam.New(sess),
		STS:   sts.New(sess),
	}
}

// IgnoreCertErrors disables TLS verification on the bundled clients. Use only
// for fault-injection scenarios — the cert scenario already covers trust path.
func (c *AWSClient) IgnoreCertErrors() {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	hc := &http.Client{Transport: tr}
	c.EC2.Config.HTTPClient = hc
	c.ELBv2.Config.HTTPClient = hc
	c.EKS.Config.HTTPClient = hc
	c.IAM.Config.HTTPClient = hc
	c.STS.Config.HTTPClient = hc
	_ = (*x509.CertPool)(nil) // silence unused-import on hardened paths
}
