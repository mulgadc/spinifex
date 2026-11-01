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
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	operatoradmin "github.com/mulgadc/spinifex/spinifex/operator/admin"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	"context"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/clusterready"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"encoding/json"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	awscreds "github.com/aws/aws-sdk-go/aws/credentials"
	awssession "github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/bedrock"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/aws/aws-sdk-go/service/sts"
	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	route53awsapi "github.com/mulgadc/spinifex/spinifex/domains/route53/awsapi"
	taggingawsapi "github.com/mulgadc/spinifex/spinifex/domains/tagging/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
	elbv2awsapi "github.com/mulgadc/spinifex/spinifex/domains/elbv2/awsapi"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	iamawsapi "github.com/mulgadc/spinifex/spinifex/domains/iam/awsapi"
	stsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sts/awsapi"
	rdsawsapi "github.com/mulgadc/spinifex/spinifex/domains/rds/awsapi"
	ecsawsapi "github.com/mulgadc/spinifex/spinifex/domains/ecs/awsapi"
	eksawsapi "github.com/mulgadc/spinifex/spinifex/domains/eks/awsapi"
	ecrdomain "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	ecrauth "github.com/mulgadc/spinifex/spinifex/domains/ecr/auth"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/policyauth"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/ec2quota"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/registryadapt"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/accountadmission"
	ingresshttp "github.com/mulgadc/spinifex/spinifex/ingress/http"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	ecrregistry "github.com/mulgadc/spinifex/spinifex/domains/ecr/registry"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/pipeline"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	ochreawsapi "github.com/mulgadc/spinifex/spinifex/domains/ochre/awsapi"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	awsdispatch "github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
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
	// spinifex/ingress/aws/pipeline/auth.go resolveLongLivedAKID).
	testAccessKeyID     = "AKIAINTEGRATIONTESTR"
	testSecretAccessKey = "integration-test-root-secret"

	// nodeDiscoverSubject is the fan-out NodeDiscovery.ActiveNodes
	// publishes to (spinifex/ingress/aws/pipeline/gateway.go). StartGateway stubs it with
	// a single-node reply so any call path that triggers node discovery
	// resolves immediately instead of riding out the fan-out's 500ms timeout.
	nodeDiscoverSubject = "spinifex.nodes.discover"

	// testECRAudience is the audience claim every harness-issued ECR JWT is
	// minted for and verified against — arbitrary but stable within this tier,
	// mirroring production's "ecr.<region>.<suffix>" shape without depending on
	// an empty registry host suffix (unset here; the in-process harness talks to
	// the gateway's httptest address directly rather than a registry hostname).
	testECRAudience = "ecr.integration-test"

	// testECRServicesDomain completes the AWS-shaped registry host ECR
	// responses advertise; no test resolves it.
	testECRServicesDomain = "integration.test"
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
	// (awsidentifiers.GlobalAccountID).
	AccountID string
	// Config is the pipeline.Config SetupRoutes was built from, exposed so
	// tests can inspect or further wire fields StartGateway did not set.
	Config *pipeline.Config
}

// Option customises a Gateway's pipeline.Config before SetupRoutes builds the
// router. Applied after StartGateway's own defaults, so an Option can
// override any of them (e.g. a different Region/AZ, or a nil IAMService to
// exercise the pre-IAM-compatibility bypass paths).
type Option func(*pipeline.Config)

// StartGateway boots the real gateway router over the package's shared,
// embedded NATS JetStream server — connected via a fresh, isolated NATS
// account so this test's state cannot collide with any other's — and serves
// it from an httptest.Server on an ephemeral port. It wires real
// IAMService/STSService implementations (not a mock) and
// seeds a root access key via SeedBootstrap, so SigV4 requests authenticate
// and authorize exactly as they would against a live environment — root
// bypasses IAM policy evaluation, matching gateway.evaluatePrincipalPolicy.
//
// ExpectedNodes is pinned to 1: natsmsg.Gather waits the FULL timeout on every
// call when ExpectedNodes is 0, which would make every stubbed NATS
// round-trip pathologically slow.
//
// Callers stub whichever NATS subjects their test needs via StubSubject;
// every other control-plane path (auth, routing, XML/JSON marshaling, IAM
// policy evaluation) runs for real.
func StartGateway(t *testing.T, opts ...Option) *Gateway {
	return startGateway(t, suiteConformance, nil, opts...)
}

// StartGatewayWithServices is StartGateway with extra service registrations,
// for a service production does not register yet.
func StartGatewayWithServices(t *testing.T, extra []awsdispatch.Registration, opts ...Option) *Gateway {
	return startGateway(t, suiteConformance, extra, opts...)
}

func startGateway(t *testing.T, collector *conformanceCollector, extra []awsdispatch.Registration, opts ...Option) *Gateway {
	t.Helper()

	// Every test connects into its own dynamically provisioned NATS account
	// on the one server this package's TestMain shares across the whole
	// binary, rather than booting a private embedded server per test — see
	// the TestMain doc comment in main_test.go for why, and how state stays
	// isolated regardless.
	nc, js, err := sharedNATSHarness.connectIsolated(accountNameFor(t))
	require.NoError(t, err)

	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)

	iamSvc, err := iamdomain.NewIAMServiceWithRetry(t.Context(), nc, masterKey)
	require.NoError(t, err)

	stsSvc, err := stsdomain.NewSTSServiceImpl(t.Context(), nc, iamSvc, masterKey, ekscluster.NewOIDCJWKSReader(js))
	require.NoError(t, err)

	encryptedSecret, err := iamdomain.EncryptSecret(testSecretAccessKey, masterKey)
	require.NoError(t, err)

	require.NoError(t, iamSvc.SeedBootstrap(&iamdomain.BootstrapData{
		AccessKeyID:     testAccessKeyID,
		EncryptedSecret: encryptedSecret,
		AccountID:       awsidentifiers.GlobalAccountID,
	}))

	// ECR auth bridge signing key: reuses the IAM master key to encrypt the
	// signing key at rest in the same embedded JetStream KV, matching
	// production's awsgw-keys wiring (runtime/roles/awsgw/awsgw.go).
	signingKey, verifyKeys, err := ecrauth.LoadOrCreateSigningKey(t.Context(), js, masterKey)
	require.NoError(t, err)

	bedrockAccess := ochre.NewModelAccessStore(js)

	// ECR's control plane is composed as the awsgw role composes it, sharing
	// the OCI registry, meta store and token issuer the /v2 data plane uses.
	ecrMeta := ecrdomain.NewNATSMetaStore(nc)
	ecrRegistry := ecrregistry.NewRegistry(objectstore.NewMemoryObjectStore(), ecrMeta, awsidentifiers.GlobalAccountID)
	ecrIssuer := ecrauth.NewIssuer(signingKey, testECRAudience)
	ecrVerifier := ecrauth.NewVerifier(verifyKeys, testECRAudience)
	ecrEndpoint := awsapi.RepositoryEndpoint{Region: testRegion, ServicesDomain: testECRServicesDomain}
	services := awsdispatch.NewBuilder()
	require.NoError(t, services.Register(awsapi.NewRegistration(awsapi.Deps{
		Registry:           awsapi.NewRegistryActionService(ecrRegistry, ecrRegistry, ecrRegistry, ecrRegistry),
		LifecyclePreview:   awsapi.NewLifecyclePreviewActionService(ecrMeta, ecrRegistry),
		Repository:         awsapi.NewRepositoryActionService(ecrMeta, ecrEndpoint),
		AuthorizationToken: awsapi.NewAuthorizationTokenActionService(ecrIssuer, ecrEndpoint),
		NATS:               nc,
	})))
	require.NoError(t, services.Register(acmawsapi.NewRegistration(acmawsapi.Deps{NATS: nc})))
	require.NoError(t, services.Register(route53awsapi.NewRegistration(route53awsapi.Deps{NATS: nc})))
	// Composed as awsgw composes them. The harness has no quota service, and
	// Tagging has no resource listers because no integration test aggregates.
	require.NoError(t, services.Register(elbv2awsapi.NewRegistration(elbv2awsapi.Deps{NATS: nc})))
	require.NoError(t, services.Register(rdsawsapi.NewRegistration(rdsawsapi.Deps{NATS: nc})))
	// Ochre model access is deny-by-default, so without a grant store every
	// bedrock route refuses. Tests sign as the system account, which the store
	// exempts from grants exactly as it does in production, so the catalog is
	// visible here without provisioning grants per test; a test signing as a
	// tenant account would have to grant first.
	require.NoError(t, services.Register(ochreawsapi.NewRegistration(ochreawsapi.Deps{Access: bedrockAccess})))
	require.NoError(t, services.Register(ecsawsapi.NewRegistration(ecsawsapi.Deps{NATS: nc})))
	require.NoError(t, services.Register(eksawsapi.NewRegistration(eksawsapi.Deps{NATS: nc, Roles: iamdomain.NewRoleResolver(iamSvc)})))
	require.NoError(t, services.Register(iamawsapi.NewRegistration(iamawsapi.Deps{
		IAM: iamSvc,
		CountInstanceProfileAssociations: func(ctx context.Context, accountID, profileARN string) (int, error) {
			return ec2instanceapi.CountInstanceProfileAssociations(ctx, nc, 1, accountID, profileARN)
		},
	})))
	stsDeps := stsawsapi.Deps{STS: stsSvc, IAM: iamSvc}
	require.NoError(t, services.Register(stsawsapi.NewRegistration(stsDeps)))
	require.NoError(t, services.Register(taggingawsapi.NewRegistration(taggingawsapi.Deps{Aggregator: tagging.NewAggregator()})))

	cfg := gwfix.WithDeps(&pipeline.Config{
		DisableLogging: true,
		Admin:          pipeline.AdminSurface{Service: operatoradmin.ServiceName, WithRequestID: operatoradmin.WithRequestID},
		Region:         testRegion,
		PreAuth:        []awsdispatch.PreAuth{stsawsapi.NewPreAuth(stsDeps)},
	}, gwfix.Deps{NATS: nc, Nodes: 1, IAM: iamSvc, STS: stsSvc})
	for _, opt := range opts {
		opt(cfg)
	}
	// EC2 is registered once the options have run so its node count follows
	// ExpectedNodes, as awsgw takes it from the same config.
	require.NoError(t, services.Register(ec2awsapi.NewRegistration(ec2awsapi.Deps{
		NATS:          nc,
		IAM:           iamSvc,
		Quota:         ec2quota.Adapter{Service: admissionquota.New(admissionquota.Limits{}, nil)},
		ActiveNodes:   func(context.Context) int { return gwfix.DepsOf(cfg).Nodes },
		ExpectedNodes: gwfix.DepsOf(cfg).Nodes,
		AZ:            testAZ,
		Endpoint:      "https://localhost:9999",
	})))
	for _, reg := range extra {
		require.NoError(t, services.Register(reg))
	}
	cfg.Services = services.Build()
	// Built after the options so a test that swaps or drops a service sees the
	// verifier follow it, as production wires the verifier over the same services.
	if cfg.Credentials == nil {
		cfg.Credentials = credverify.New(gwfix.DepsOf(cfg).IAM, gwfix.DepsOf(cfg).STS)
	}

	// The OCI /v2 data plane. Its Meta store is a NATS client, so a test that
	// pushes or pulls must call StartECRDaemonLite or every request times out.
	// Blob and manifest bytes are memory-backed: no predastore.
	if cfg.Policy == nil {
		cfg.Policy = policyauth.New(gwfix.DepsOf(cfg).IAM)
	}
	if cfg.Readiness == nil {
		cfg.Readiness = clusterready.New(gwfix.DepsOf(cfg).NATS)
	}
	admission := accountadmission.New(gwfix.DepsOf(cfg).IAM)
	if cfg.Admission == nil {
		cfg.Admission = admission
	}
	if cfg.PublicRoutes == nil {
		bridge, err := ecrregistry.NewBridge(registryadapt.Config(ecrregistry.BridgeConfig{
			Registry:  ecrRegistry,
			Verifier:  ecrVerifier,
			Issuer:    ecrIssuer,
			Admission: admission,
			Region:    cfg.Region,
		}, credverify.New(gwfix.DepsOf(cfg).IAM, gwfix.DepsOf(cfg).STS), policyauth.New(gwfix.DepsOf(cfg).IAM)))
		require.NoError(t, err)
		cfg.PublicRoutes = awsgw.PublicRoutes(nil, ingresshttp.NewHostMatcher("", "dkr", "ecr").Middleware(bridge.Handler()))
	}

	handler := cfg.SetupRoutes()
	if collector != nil {
		handler = conformanceMiddleware(handler, collector)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	gw := &Gateway{
		Server:    srv,
		NATSConn:  nc,
		AccountID: awsidentifiers.GlobalAccountID,
		Config:    cfg,
	}

	nodeReply, err := json.Marshal(clusterv1.NodeDiscoverResponse{Node: "integration-test-node"})
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
// catalog logic (domains/ochre/catalog.go) with no NATS/daemon hop, so no
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
	// Route53 is global; the gateway accepts its signature in any region.
	Route53 *route53.Route53
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
		EC2:     ec2.New(sess),
		IAM:     iam.New(sess),
		STS:     sts.New(sess),
		ECR:     ecr.New(sess),
		Route53: route53.New(sess),
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
