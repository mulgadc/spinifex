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
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
)

// AWSClient bundles the SDK service handles for the local Spinifex AWS gateway.
// Region defaults to ap-southeast-2; override via SPINIFEX_AWS_REGION.
type AWSClient struct {
	// EC2 is wrapped so RunInstances auto-serialises against cluster capacity
	// (see capacityRetryEC2). EC2Conf is the underlying SDK client shared by
	// both; kept for call sites that read or mutate its Config.
	EC2     ec2iface.EC2API
	EC2Conf *ec2.EC2
	ELBv2   *elbv2.ELBV2
	IAM     *iam.IAM
	STS     *sts.STS
	EKS     *eks.EKS
	ACM     *acm.ACM
}

// NewAWSClient builds clients pointed at the spinifex gateway using the
// spinifex CA. Credentials come from AWS_PROFILE (default "spinifex") or
// SPINIFEX_AWS_ACCESS_KEY_ID / SPINIFEX_AWS_SECRET_ACCESS_KEY.
func NewAWSClient(t *testing.T, env *Env) *AWSClient {
	t.Helper()
	id, secret := os.Getenv("SPINIFEX_AWS_ACCESS_KEY_ID"), os.Getenv("SPINIFEX_AWS_SECRET_ACCESS_KEY")
	return newAWSClient(t, env, id, secret, "")
}

// NewAWSClientWithCreds builds an AWSClient with explicit static credentials,
// bypassing AWS_PROFILE lookup.
func NewAWSClientWithCreds(t *testing.T, env *Env, accessKey, secretKey string) *AWSClient {
	t.Helper()
	if accessKey == "" || secretKey == "" {
		t.Fatalf("NewAWSClientWithCreds: empty credentials")
	}
	return newAWSClient(t, env, accessKey, secretKey, "")
}

// NewAWSClientWithSessionCreds builds an AWSClient with STS temporary
// credentials, signing requests with the session token (ASIA auth path).
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

	// SPINIFEX_AWS_INSECURE=1 skips CA load for runner-resident scenarios that
	// lack the spinifex cert on disk. Trust is covered by the cert suite.
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
		// Static creds: used for per-tenant carousel profiles and STS session creds.
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

	rawEC2 := ec2.New(sess)
	return &AWSClient{
		EC2:     &capacityRetryEC2{EC2API: rawEC2},
		EC2Conf: rawEC2,
		ELBv2:   elbv2.New(sess),
		IAM:     iam.New(sess),
		STS:     sts.New(sess),
		EKS:     eks.New(sess),
		ACM:     acm.New(sess),
	}
}

// IgnoreCertErrors disables TLS verification on all bundled clients.
// Use only for fault-injection scenarios.
func (c *AWSClient) IgnoreCertErrors() {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	hc := &http.Client{Transport: tr}
	c.EC2Conf.Config.HTTPClient = hc
	c.ELBv2.Config.HTTPClient = hc
	c.EKS.Config.HTTPClient = hc
	c.IAM.Config.HTTPClient = hc
	c.STS.Config.HTTPClient = hc
	c.ACM.Config.HTTPClient = hc
	_ = (*x509.CertPool)(nil) // silence unused-import on hardened paths
}
