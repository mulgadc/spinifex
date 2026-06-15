package handlers_imds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/service/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
)

// tokenSweepInterval is how often abandoned/expired IMDSv2 tokens are swept.
const tokenSweepInterval = time.Minute

// profileLookup is the IAMService slice needed for credential + iam/* paths.
type profileLookup interface {
	ResolveInstanceProfile(accountID, nameOrARN string) (*handlers_iam.InstanceProfile, error)
	GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error)
}

// publicKeyLookup fetches the SSH public key material for the public-keys path.
type publicKeyLookup interface {
	GetPublicKey(accountID, keyName string) (string, error)
}

// IMDSService is the host-served EC2 Instance Metadata Service (169.254.169.254).
type IMDSService interface {
	Run(ctx context.Context) error
}

var _ IMDSService = (*IMDSServiceImpl)(nil)

// eniResolver maps a datapath-attested (vpcID, srcIP) to ENI + instance facts.
type eniResolver interface {
	resolveENI(vpcID, srcIP string) (*eniFacts, error)
	resolveInstance(eni *eniFacts) (*instanceFacts, error)
	resolveSGNames(accountID string, sgIDs []string) []string
}

// IMDSServiceImpl is the in-process IMDS implementation with its own per-subnet listener stack.
type IMDSServiceImpl struct {
	resolver eniResolver
	tokens   *tokenStore
	creds    *credCache
	iam      profileLookup
	pubKeys  publicKeyLookup
	bind     *bindManager
	now      func() time.Time
}

// NewIMDSServiceImpl wires the IMDS service. ensureVeth/removeVeth are injected to avoid an import cycle.
func NewIMDSServiceImpl(natsConn *nats.Conn, sts stsAssumer, iamSvc profileLookup, pubKeys publicKeyLookup, expectedNodes int, ensureVeth ensureVethFunc, removeVeth removeVethFunc) (*IMDSServiceImpl, error) {
	if natsConn == nil {
		return nil, errors.New("nil NATS connection")
	}
	if sts == nil {
		return nil, errors.New("nil STS service")
	}
	if iamSvc == nil {
		return nil, errors.New("nil IAM service")
	}
	if pubKeys == nil {
		return nil, errors.New("nil public key service")
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
	vethKV, err := js.KeyValue(KVBucketIMDSSubnetVeth)
	if err != nil {
		return nil, fmt.Errorf("open %s bucket: %w", KVBucketIMDSSubnetVeth, err)
	}

	// Open SG bucket best-effort; IMDS starts fine without it, degrading to raw IDs.
	sgKV, err := js.KeyValue(kvBucketSecurityGroups)
	if err != nil {
		slog.Warn("IMDS: security-group bucket unavailable, /security-groups will serve IDs", "bucket", kvBucketSecurityGroups, "err", err)
		sgKV = nil
	}

	svc := &IMDSServiceImpl{
		resolver: &metadataResolver{
			index:  indexKV,
			eniKV:  eniKV,
			sgKV:   sgKV,
			lookup: &natsInstanceLookup{nc: natsConn, expectedNodes: expectedNodes},
		},
		tokens:  newTokenStore(),
		creds:   newCredCache(sts),
		iam:     iamSvc,
		pubKeys: pubKeys,
		now:     time.Now,
	}
	svc.bind = newBindManager(vethKV, svc.httpHandler(), ensureVeth, removeVeth, bindLocalListener)

	slog.Info("IMDS service initialized",
		"index_bucket", KVBucketENIByVPCIP,
		"veth_bucket", KVBucketIMDSSubnetVeth)
	return svc, nil
}

// httpHandler builds the shared mux for every per-subnet listener.
func (s *IMDSServiceImpl) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pathToken, s.handleToken)
	mux.HandleFunc("/", s.handleMetadata)
	return rejectForwarded(mux)
}

// Run starts the bind manager (sync + watch) and token-sweep ticker, blocks until ctx is done.
// Initial sync failure is non-fatal; vpcd readiness must not be gated on IMDS.
func (s *IMDSServiceImpl) Run(ctx context.Context) error {
	if err := s.bind.sync(ctx); err != nil {
		slog.Warn("IMDS: initial bind sync failed, continuing", "err", err)
	}
	go s.bind.watch(ctx)
	go s.sweepExpired(ctx)

	<-ctx.Done()
	s.bind.shutdown()
	return ctx.Err()
}

// sweepExpired evicts expired tokens and stale credential-cache entries on a ticker.
func (s *IMDSServiceImpl) sweepExpired(ctx context.Context) {
	ticker := time.NewTicker(tokenSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := s.now()
			s.tokens.sweep(now)
			s.creds.sweep(now)
		}
	}
}
