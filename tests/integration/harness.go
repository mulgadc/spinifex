//go:build integration

// Package integration is the in-process integration test tier for spinifex's
// AWS-compatible control plane. Tests here start the REAL gateway router
// (gateway.SetupRoutes) against an embedded NATS JetStream instance shared
// across the whole package (see TestMain in main_test.go), with real IAM/STS
// services wired in for authentic SigV4 authz — the only thing faked is the
// daemon side of the wire: the NATS subjects a live spinifex daemon would
// answer (ec2.*, ebs.*, ...) are stubbed per-test via StubSubject.
//
// Nothing is provisioned: no tofu, no docker, no Spinifex daemons, no fixed
// ports. Every test connects into its own isolated NATS account on the
// shared server (see TestMain), so tests stay independent in state despite
// sharing the underlying process, and are safe to run concurrently on a
// shared build host.
//
// This package is gated behind the "integration" build tag (see the
// `test-integration` Makefile target) so it is never swept by the default
// `go test ./spinifex/...`, `make test-cover`, or `make test-race`.
package integration

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	awscreds "github.com/aws/aws-sdk-go/aws/credentials"
	awssession "github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	gateway_ecr "github.com/mulgadc/spinifex/spinifex/gateway/ecr"
	gateway_ecrauth "github.com/mulgadc/spinifex/spinifex/gateway/ecrauth"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/types"
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

	// nodeDiscoverSubject is the fan-out GatewayConfig.DiscoverActiveNodes
	// publishes to (spinifex/gateway/gateway.go). StartGateway stubs it with
	// a single-node reply so any call path that triggers node discovery
	// resolves immediately instead of riding out the fan-out's 500ms timeout.
	nodeDiscoverSubject = "spinifex.nodes.discover"

	// testECRAudience is the audience claim every harness-issued ECR JWT is
	// minted for and verified against — arbitrary but stable within this tier,
	// mirroring production's "ecr.<region>.<suffix>" shape without depending on
	// GatewayConfig.InternalSuffix (unset here; the in-process harness talks to
	// the gateway's httptest address directly rather than a registry hostname).
	testECRAudience = "ecr.integration-test"
)

// Gateway is a running in-process instance of the real AWS gateway router,
// backed by the package's shared embedded NATS JetStream server and real
// IAM/STS services. Obtained via StartGateway; the httptest server and any
// StubSubject responders registered on it are torn down automatically via
// t.Cleanup. NATSConn is NOT closed at test end — see the TestMain doc
// comment in main_test.go for why — TestMain closes every test's connection
// together once the whole package's tests have finished.
type Gateway struct {
	// Server hosts gateway.SetupRoutes() on an ephemeral httptest port.
	Server *httptest.Server
	// NATSConn is this test's isolated JetStream connection into the shared
	// server (see TestMain), which the gateway, IAMService, and STSService
	// all share. Tests use it directly with StubSubject to fake daemon-side
	// responders.
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

// StartGateway boots the real gateway router over the package's shared,
// embedded NATS JetStream server — connected via a fresh, isolated NATS
// account so this test's state cannot collide with any other's — and serves
// it from an httptest.Server on an ephemeral port. It wires real
// IAMService/STSService implementations (not a mock) and
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

	// Every test connects into its own dynamically provisioned NATS account
	// on the one server this package's TestMain shares across the whole
	// binary, rather than booting a private embedded server per test — see
	// the TestMain doc comment in main_test.go for why, and how state stays
	// isolated regardless.
	nc, js, err := sharedNATSHarness.connectIsolated(accountNameFor(t))
	require.NoError(t, err)

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

	// ECR auth bridge signing key: reuses the IAM master key to encrypt the
	// signing key at rest in the same embedded JetStream KV, matching
	// production's awsgw-keys wiring (services/awsgw/awsgw.go).
	signingKey, verifyKeys, err := gateway_ecrauth.LoadOrCreateSigningKey(js, masterKey, 1)
	require.NoError(t, err)

	cfg := &gateway.GatewayConfig{
		DisableLogging: true,
		NATSConn:       nc,
		ExpectedNodes:  1,
		Region:         testRegion,
		AZ:             testAZ,
		IAMService:     iamSvc,
		STSService:     stsSvc,
		// ECRRegistry serves the OCI Distribution v2 (/v2/*) data plane; its
		// Meta store is a NATS client, so a test that pushes/pulls or manages
		// repositories must additionally call StartECRDaemonLite to subscribe a
		// real MetaServiceImpl or every ECR request will time out with no
		// responder. Blob/manifest bytes are memory-backed: no predastore.
		ECRRegistry:      gateway_ecr.NewRegistry(objectstore.NewMemoryObjectStore(), handlers_ecr.NewNATSMetaStore(nc), utils.GlobalAccountID),
		ECRTokenIssuer:   gateway_ecrauth.NewIssuer(signingKey, testECRAudience),
		ECRTokenVerifier: gateway_ecrauth.NewVerifier(verifyKeys, testECRAudience),
	}
	for _, opt := range opts {
		opt(cfg)
	}

	srv := httptest.NewServer(cfg.SetupRoutes())
	t.Cleanup(srv.Close)

	gw := &Gateway{
		Server:    srv,
		NATSConn:  nc,
		AccountID: utils.GlobalAccountID,
		Config:    cfg,
	}

	nodeReply, err := json.Marshal(types.NodeDiscoverResponse{Node: "integration-test-node"})
	require.NoError(t, err)
	gw.StubSubject(t, nodeDiscoverSubject, nodeReply)

	return gw
}

// EC2Client returns an aws-sdk-go v1 EC2 client pointed at the gateway's
// httptest server, signed with the seeded root credentials over plain HTTP
// (the httptest server is not TLS).
func (gw *Gateway) EC2Client(t *testing.T) *ec2.EC2 {
	t.Helper()
	return ec2.New(gw.session(t))
}

// IAMClient returns an aws-sdk-go v1 IAM client pointed at the gateway's
// httptest server, signed with the seeded root credentials.
func (gw *Gateway) IAMClient(t *testing.T) *iam.IAM {
	t.Helper()
	return iam.New(gw.session(t))
}

// STSClient returns an aws-sdk-go v1 STS client pointed at the gateway's
// httptest server, signed with the seeded root credentials.
func (gw *Gateway) STSClient(t *testing.T) *sts.STS {
	t.Helper()
	return sts.New(gw.session(t))
}

// ECRClient returns an aws-sdk-go v1 ECR client pointed at the gateway's
// httptest server, signed with the seeded root credentials. Repository and
// image operations dispatch to ECRRegistry.Meta over NATS, so a test needs
// StartECRDaemonLite running before calling anything beyond
// GetAuthorizationToken.
func (gw *Gateway) ECRClient(t *testing.T) *ecr.ECR {
	t.Helper()
	return ecr.New(gw.session(t))
}

// BedrockClient returns an aws-sdk-go v1 Bedrock (control-plane) client
// pointed at the gateway's httptest server, signed with the seeded root
// credentials. ListFoundationModels/GetFoundationModel are pure gateway-side
// catalog logic (gateway/bedrock/catalog.go) with no NATS/daemon hop, so no
// DaemonLite wiring is needed to exercise them.
func (gw *Gateway) BedrockClient(t *testing.T) *bedrock.Bedrock {
	t.Helper()
	return bedrock.New(gw.session(t))
}

// PrincipalClients bundles the SDK service handles an authz test needs to act
// as a principal other than the harness's seeded root user: an IAM user
// created within the test, or a role assumed via STS. Populated by
// ClientsWithCreds / ClientsWithSessionCreds.
type PrincipalClients struct {
	EC2 *ec2.EC2
	IAM *iam.IAM
	STS *sts.STS
	ECR *ecr.ECR
}

// ClientsWithCreds returns PrincipalClients signed with a static access
// key/secret and no session token — the long-lived-access-key path (an AKIA
// key from IAM CreateAccessKey), as opposed to the seeded-root session
// EC2Client/IAMClient/STSClient use.
func (gw *Gateway) ClientsWithCreds(t *testing.T, accessKeyID, secretAccessKey string) *PrincipalClients {
	t.Helper()
	return gw.principalClients(t, accessKeyID, secretAccessKey, "")
}

// ClientsWithSessionCreds returns PrincipalClients signed with STS temporary
// credentials — the assumed-role path (an ASIA key, secret, and session
// token from STS AssumeRole).
func (gw *Gateway) ClientsWithSessionCreds(t *testing.T, accessKeyID, secretAccessKey, sessionToken string) *PrincipalClients {
	t.Helper()
	return gw.principalClients(t, accessKeyID, secretAccessKey, sessionToken)
}

// principalClients builds the session backing both ClientsWithCreds and
// ClientsWithSessionCreds. Passing sessionToken == "" here — the
// ClientsWithCreds path — must not add an X-Amz-Security-Token header at
// all: aws-sdk-go's v4 signer only sets the header when SessionToken is
// non-empty, so a single code path correctly serves both the long-lived-key
// and assumed-role principals without a conditional.
func (gw *Gateway) principalClients(t *testing.T, accessKeyID, secretAccessKey, sessionToken string) *PrincipalClients {
	t.Helper()
	sess, err := awssession.NewSession(&aws.Config{
		Region:      aws.String(testRegion),
		Endpoint:    aws.String(gw.Server.URL),
		Credentials: awscreds.NewStaticCredentials(accessKeyID, secretAccessKey, sessionToken),
		DisableSSL:  aws.Bool(true),
		// Error-path tests assert on the first response; the default retry
		// loop would mask a deterministic 4xx behind minutes of backoff.
		MaxRetries: aws.Int(0),
	})
	require.NoError(t, err)
	return &PrincipalClients{
		EC2: ec2.New(sess),
		IAM: iam.New(sess),
		STS: sts.New(sess),
		ECR: ecr.New(sess),
	}
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
