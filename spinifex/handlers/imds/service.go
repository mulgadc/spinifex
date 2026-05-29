package handlers_imds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
)

// tokenSweepInterval is how often abandoned/expired IMDSv2 tokens are swept.
const tokenSweepInterval = time.Minute

// profileLookup is the slice of IAMService the IMDS credential + iam/* paths
// need: dereference an instance-profile ARN to its record, and resolve a role
// name to its canonical ARN. Narrowed to an interface for unit testing.
type profileLookup interface {
	ResolveInstanceProfile(accountID, nameOrARN string) (*handlers_iam.InstanceProfile, error)
	GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error)
}

// IMDSService is the host-served EC2 Instance Metadata Service. It runs in
// awsgw on every chassis, serving 169.254.169.254 to local guest VMs over
// per-VPC veths. Run owns the listener lifecycle and blocks until ctx is done.
type IMDSService interface {
	Run(ctx context.Context) error
}

var _ IMDSService = (*IMDSServiceImpl)(nil)

// eniResolver maps a datapath-attested (vpcID, srcIP) to ENI + instance facts.
// An interface so the HTTP handlers are unit-testable without a live KV;
// metadataResolver is the production implementation.
type eniResolver interface {
	resolveENI(vpcID, srcIP string) (*eniFacts, error)
	resolveInstance(eni *eniFacts) (*instanceFacts, error)
}

// IMDSServiceImpl is the in-process IMDS implementation. It is not part of the
// HTTPS gateway dispatch surface — it owns its own per-VPC listener stack.
type IMDSServiceImpl struct {
	resolver eniResolver
	tokens   *tokenStore
	creds    *credCache
	iam      profileLookup
	bind     *bindManager
	now      func() time.Time
}

// NewIMDSServiceImpl wires the IMDS service. natsConn backs both the KV reads
// (eni-by-vpc-ip index, ENI bucket, vpc-veth bucket) and the account-scoped
// instance fan-out; expectedNodes sizes that fan-out's response collection.
//
// ensureVeth / removeVeth are injected rather than imported: the host helpers
// (host.EnsureIMDSVeth / host.RemoveIMDSVeth) live in network/host, which
// transitively imports this package, so awsgw — which can import both — passes
// them in. Tests inject fakes.
func NewIMDSServiceImpl(natsConn *nats.Conn, sts stsAssumer, iamSvc profileLookup, expectedNodes int, ensureVeth ensureVethFunc, removeVeth removeVethFunc) (*IMDSServiceImpl, error) {
	if natsConn == nil {
		return nil, errors.New("nil NATS connection")
	}
	if sts == nil {
		return nil, errors.New("nil STS service")
	}
	if iamSvc == nil {
		return nil, errors.New("nil IAM service")
	}
	if ensureVeth == nil || removeVeth == nil {
		return nil, errors.New("nil veth lifecycle hooks")
	}

	js, err := natsConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("get JetStream context: %w", err)
	}

	indexKV, err := js.KeyValue(KVBucketENIByVPCIP)
	if err != nil {
		return nil, fmt.Errorf("open %s bucket: %w", KVBucketENIByVPCIP, err)
	}
	eniKV, err := js.KeyValue(kvBucketENIs)
	if err != nil {
		return nil, fmt.Errorf("open %s bucket: %w", kvBucketENIs, err)
	}
	vethKV, err := js.KeyValue(KVBucketIMDSVPCVeth)
	if err != nil {
		return nil, fmt.Errorf("open %s bucket: %w", KVBucketIMDSVPCVeth, err)
	}

	svc := &IMDSServiceImpl{
		resolver: &metadataResolver{
			index:  indexKV,
			eniKV:  eniKV,
			lookup: &natsInstanceLookup{nc: natsConn, expectedNodes: expectedNodes},
		},
		tokens: newTokenStore(),
		creds:  newCredCache(sts),
		iam:    iamSvc,
		now:    time.Now,
	}
	svc.bind = newBindManager(vethKV, svc.httpHandler(), ensureVeth, removeVeth, bindLocalListener)

	slog.Info("IMDS service initialized",
		"index_bucket", KVBucketENIByVPCIP,
		"veth_bucket", KVBucketIMDSVPCVeth)
	return svc, nil
}

// httpHandler builds the shared mux used by every per-VPC listener. The token
// PUT is the only path reachable without a token; everything else is gated.
func (s *IMDSServiceImpl) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pathToken, s.handleToken)
	mux.HandleFunc("/", s.handleMetadata)
	return mux
}

// Run starts the bind manager (sync + watch) and the token-sweep ticker, then
// blocks until ctx is cancelled. The initial sync failing is logged but not
// fatal — other awsgw services must not have their readiness gated on IMDS
// plumbing, and SDKs retry while it converges.
func (s *IMDSServiceImpl) Run(ctx context.Context) error {
	if err := s.bind.sync(ctx); err != nil {
		slog.Warn("IMDS: initial bind sync failed, continuing", "err", err)
	}
	go s.bind.watch(ctx)
	go s.sweepTokens(ctx)

	<-ctx.Done()
	s.bind.shutdown()
	return ctx.Err()
}

// sweepTokens evicts expired tokens on a ticker for the life of ctx.
func (s *IMDSServiceImpl) sweepTokens(ctx context.Context) {
	ticker := time.NewTicker(tokenSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tokens.sweep(s.now())
		}
	}
}

// getRoleInput builds the GetRole input for a role name.
func getRoleInput(roleName string) *iam.GetRoleInput {
	return &iam.GetRoleInput{RoleName: aws.String(roleName)}
}
