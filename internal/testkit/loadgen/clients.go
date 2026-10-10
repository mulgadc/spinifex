package loadgen

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/aws/aws-sdk-go/service/sts/stsiface"
)

// Clients holds one tenant's SDK clients. They are built once and reused for
// the whole run: rebuilding a session per request would measure session
// construction, and holding a connection is what a real SDK client does.
type Clients struct {
	EC2 ec2iface.EC2API
	STS stsiface.STSAPI
}

// Dial is what to point the clients at.
type Dial struct {
	Endpoint string
	Region   string
	// CABundle is needed only when the endpoint serves the cluster's own
	// certificate. Through a publicly trusted proxy it is empty and system
	// trust applies.
	CABundle string
	// MaxIdleConnsPerHost has to be at least the stage concurrency, or the
	// transport closes and reopens connections under load and the run measures
	// TLS handshakes.
	MaxIdleConnsPerHost int
	Timeout             time.Duration
}

// NewClients builds one tenant's clients from a shared-config profile.
func NewClients(dial Dial, profile string) (*Clients, error) {
	if profile == "" {
		return nil, errors.New("loadgen: profile is required")
	}

	httpClient, err := httpClientFor(dial)
	if err != nil {
		return nil, err
	}

	sess, err := session.NewSessionWithOptions(session.Options{
		Profile:           profile,
		SharedConfigState: session.SharedConfigEnable,
		Config: aws.Config{
			Endpoint:         aws.String(dial.Endpoint),
			Region:           aws.String(dial.Region),
			S3ForcePathStyle: aws.Bool(true),
			HTTPClient:       httpClient,
			MaxRetries:       aws.Int(0),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("loadgen: session for profile %s: %w", profile, err)
	}

	return &Clients{EC2: ec2.New(sess), STS: sts.New(sess)}, nil
}

// httpClientFor builds the transport every client shares. Retries are off in
// the session above on purpose: a retried request is a slow request, and
// hiding it inside one sample turns a failing cluster into a fast one.
func httpClientFor(dial Dial) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if dial.CABundle != "" {
		pem, err := os.ReadFile(dial.CABundle)
		if err != nil {
			return nil, fmt.Errorf("loadgen: read CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("loadgen: no certificates in %s", dial.CABundle)
		}
		tlsConfig.RootCAs = pool
	}

	idle := dial.MaxIdleConnsPerHost
	if idle < 1 {
		idle = 64
	}
	timeout := dial.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig:     tlsConfig,
			MaxIdleConns:        idle * 2,
			MaxIdleConnsPerHost: idle,
			IdleConnTimeout:     90 * time.Second,
		},
	}, nil
}

// ErrorCode classifies a failure for the error table. An AWS error keeps its
// service code so a 500 and a throttle never merge into one number; anything
// else is reported by its Go type, which is enough to tell a timeout from a
// connection refusal.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if awsErr, ok := errors.AsType[awserr.Error](err); ok {
		return awsErr.Code()
	}
	if errors.Is(err, ErrShed) {
		return shedCode
	}
	return fmt.Sprintf("%T", err)
}
