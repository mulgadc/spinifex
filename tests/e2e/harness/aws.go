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
	"github.com/aws/aws-sdk-go/service/elbv2"
)

// AWSClient bundles the SDK service handles a scenario typically needs against
// the local Spinifex AWS gateway. Region is fixed to ap-southeast-2 to match
// the bash scripts; override via SPINIFEX_AWS_REGION.
type AWSClient struct {
	EC2   *ec2.EC2
	ELBv2 *elbv2.ELBV2
}

// NewAWSClient builds clients pointed at https://<endpoint>:<port>/ using the
// spinifex CA for TLS verification. Credentials come from the AWS_PROFILE env
// var (matching the bash scripts) — default `spinifex`. Override via
// SPINIFEX_AWS_ACCESS_KEY_ID / SPINIFEX_AWS_SECRET_ACCESS_KEY for static creds.
func NewAWSClient(t *testing.T, env *Env) *AWSClient {
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

	caPath, err := ResolveCACert(env)
	if err != nil {
		t.Fatalf("AWS client: %v", err)
	}
	pool, err := LoadCAPool(caPath)
	if err != nil {
		t.Fatalf("AWS client: %v", err)
	}

	cfg := &aws.Config{
		Endpoint:         aws.String(endpoint),
		Region:           aws.String(region),
		S3ForcePathStyle: aws.Bool(true),
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool},
			},
		},
	}

	if id, secret := os.Getenv("SPINIFEX_AWS_ACCESS_KEY_ID"), os.Getenv("SPINIFEX_AWS_SECRET_ACCESS_KEY"); id != "" && secret != "" {
		cfg.Credentials = credentials.NewStaticCredentials(id, secret, "")
	}

	sess, err := session.NewSessionWithOptions(session.Options{
		Config:            *cfg,
		SharedConfigState: session.SharedConfigEnable,
		Profile:           getenv("AWS_PROFILE", "spinifex"),
	})
	if err != nil {
		t.Fatalf("AWS session: %v", err)
	}

	return &AWSClient{
		EC2:   ec2.New(sess),
		ELBv2: elbv2.New(sess),
	}
}

// IgnoreCertErrors disables TLS verification on the bundled clients. Use only
// for fault-injection scenarios — the cert scenario already covers trust path.
func (c *AWSClient) IgnoreCertErrors() {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	c.EC2.Config.HTTPClient = &http.Client{Transport: tr}
	c.ELBv2.Config.HTTPClient = &http.Client{Transport: tr}
	_ = (*x509.CertPool)(nil) // silence unused-import on hardened paths
}
