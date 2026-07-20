//go:build integration

// Package integration is the in-process integration test tier for spinifex's
// AWS-compatible control plane. Tests here start the REAL gateway router
// (gateway.SetupRoutes) against an embedded, ephemeral NATS JetStream
// instance, with real IAM/STS services wired in for authentic SigV4 authz —
// the only thing faked is the daemon side of the wire: the NATS subjects a
// live spinifex daemon would answer (ec2.*, ebs.*, ...) are stubbed per-test
// via StubSubject.
//
// Nothing is provisioned: no tofu, no docker, no Spinifex daemons, no fixed
// ports. Every dependency starts ephemeral and is torn down via t.Cleanup, so
// this tier is safe to run as a fast CI front gate ahead of the live E2E
// suites, and safe to run concurrently on a shared build host.
//
// This package is gated behind the "integration" build tag (see the
// `test-integration` Makefile target) so it is never swept by the default
// `go test ./spinifex/...`, `make test-cover`, or `make test-race`.
package integration

import (
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	awscreds "github.com/aws/aws-sdk-go/aws/credentials"
	awssession "github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

const (
	// testRegion/testAZ pin the gateway's SigV4 credential scope and AZ.
	// Every SDK client built via Gateway.EC2Client signs against testRegion,
	// so requests verify against the same scope the gateway expects.
	testRegion = "us-east-1"
	testAZ     = "us-east-1a"

	// testAccessKeyID/testSecretAccessKey are the root credentials seeded
	// into the real IAM service for every harness-started gateway. AKIA
	// prefix routes SigV4 auth through the long-lived-key path (see
	// spinifex/gateway/auth.go resolveLongLivedAKID).
	testAccessKeyID     = "AKIAINTEGRATIONTESTR"
	testSecretAccessKey = "integration-test-root-secret"
)

// Gateway is a running in-process instance of the real AWS gateway router,
// backed by embedded NATS JetStream and real IAM/STS services. Obtained via
// StartGateway; every resource it owns is torn down automatically via
// t.Cleanup, including the NATS server, the httptest server, and any
// StubSubject responders registered on it.
type Gateway struct {
	// Server hosts gateway.SetupRoutes() on an ephemeral httptest port.
	Server *httptest.Server
	// NATSConn is the embedded JetStream connection the gateway, IAMService,
	// and STSService all share. Tests use it directly with StubSubject to
	// fake daemon-side responders.
	NATSConn *nats.Conn
	// AccountID is the account the seeded root credentials belong to
	// (utils.GlobalAccountID).
	AccountID string
	// Config is the GatewayConfig SetupRoutes was built from, exposed so
	// tests can inspect or further wire fields StartGateway did not set.
	Config *gateway.GatewayConfig
}

// Option customises a Gateway's GatewayConfig before SetupRoutes builds the
// router. Applied after StartGateway's own defaults, so an Option can
// override any of them (e.g. a different Region/AZ, or a nil IAMService to
// exercise the pre-IAM-compatibility bypass paths).
type Option func(*gateway.GatewayConfig)

// StartGateway boots the real gateway router over an embedded, ephemeral NATS
// JetStream instance and serves it from an httptest.Server on an ephemeral
// port. It wires real IAMService/STSService implementations (not a mock) and
// seeds a root access key via SeedBootstrap, so SigV4 requests authenticate
// and authorize exactly as they would against a live environment — root
// bypasses IAM policy evaluation, matching gateway.evaluatePrincipalPolicy.
//
// ExpectedNodes is pinned to 1: utils.Gather waits the FULL timeout on every
// call when ExpectedNodes is 0, which would make every stubbed NATS
// round-trip pathologically slow.
//
// Callers stub whichever NATS subjects their test needs via StubSubject;
// every other control-plane path (auth, routing, XML/JSON marshaling, IAM
// policy evaluation) runs for real.
func StartGateway(t *testing.T, opts ...Option) *Gateway {
	t.Helper()

	_, nc, _ := testutil.StartTestJetStream(t)

	masterKey, err := handlers_iam.GenerateMasterKey()
	require.NoError(t, err)

	iamSvc, err := handlers_iam.NewIAMServiceWithRetry(nc, masterKey, 1)
	require.NoError(t, err)

	stsSvc, err := handlers_sts.NewSTSServiceImpl(nc, iamSvc, masterKey, 1)
	require.NoError(t, err)

	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretAccessKey, masterKey)
	require.NoError(t, err)

	require.NoError(t, iamSvc.SeedBootstrap(&handlers_iam.BootstrapData{
		AccessKeyID:     testAccessKeyID,
		EncryptedSecret: encryptedSecret,
		AccountID:       utils.GlobalAccountID,
	}))

	cfg := &gateway.GatewayConfig{
		DisableLogging: true,
		NATSConn:       nc,
		ExpectedNodes:  1,
		Region:         testRegion,
		AZ:             testAZ,
		IAMService:     iamSvc,
		STSService:     stsSvc,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	srv := httptest.NewServer(cfg.SetupRoutes())
	t.Cleanup(srv.Close)

	return &Gateway{
		Server:    srv,
		NATSConn:  nc,
		AccountID: utils.GlobalAccountID,
		Config:    cfg,
	}
}

// EC2Client returns an aws-sdk-go v1 EC2 client pointed at the gateway's
// httptest server, signed with the seeded root credentials over plain HTTP
// (the httptest server is not TLS).
func (gw *Gateway) EC2Client(t *testing.T) *ec2.EC2 {
	t.Helper()
	return ec2.New(gw.session(t))
}

// session builds the shared aws-sdk-go v1 Session every per-service client
// accessor (EC2Client, ...) derives from, pointed at this Gateway's httptest
// server and signed with the seeded root credentials.
func (gw *Gateway) session(t *testing.T) *awssession.Session {
	t.Helper()
	sess, err := awssession.NewSession(&aws.Config{
		Region:      aws.String(testRegion),
		Endpoint:    aws.String(gw.Server.URL),
		Credentials: awscreds.NewStaticCredentials(testAccessKeyID, testSecretAccessKey, ""),
		DisableSSL:  aws.Bool(true),
		// Error-path tests assert on the first response; the default retry
		// loop would mask a deterministic 4xx behind minutes of backoff.
		MaxRetries: aws.Int(0),
	})
	require.NoError(t, err)
	return sess
}
