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

// tapReconcileInterval is how often vpcd reconciles its per-tap responders against
// the live OVS tap set. It bounds the post-boot window before a freshly launched
// guest's responder begins serving; cloud-init's IMDS retries absorb that gap.
const tapReconcileInterval = 15 * time.Second

// listTapsFunc enumerates the local primary-ENI IMDS taps as eniID → endpoint,
// injected (like the veth hooks) to keep handlers/imds free of a network/host
// import cycle. Backed by host.ListIMDSTaps in vpcd.
type listTapsFunc func(ctx context.Context) (map[string]string, error)

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

// eniResolver maps a request to ENI + instance facts, either from a datapath-
// attested (vpcID, srcIP) (per-subnet localport) or from the tap's ENI ID
// (per-tap responder).
type eniResolver interface {
	resolveENI(vpcID, srcIP string) (*eniFacts, error)
	resolveENIByID(eniID string) (*eniFacts, error)
	resolveInstance(eni *eniFacts) (*instanceFacts, error)
	resolveSGNames(accountID string, sgIDs []string) []string
}

// IMDSServiceImpl is the in-process IMDS implementation. It carries both the
// per-subnet localport listener stack (live) and the per-tap responder manager
// (built but off the production path until the Phase 3 cutover).
type IMDSServiceImpl struct {
	resolver eniResolver
	tokens   *tokenStore
	creds    *credCache
	iam      profileLookup
	pubKeys  publicKeyLookup
	bind     *bindManager
	tapResp  *tapResponderManager
	listTaps listTapsFunc
	now      func() time.Time
}

// NewIMDSServiceImpl wires the IMDS service. ensureVeth/removeVeth and listTaps
// are injected to avoid a network/host import cycle.
func NewIMDSServiceImpl(natsConn *nats.Conn, sts stsAssumer, iamSvc profileLookup, pubKeys publicKeyLookup, expectedNodes int, ensureVeth ensureVethFunc, removeVeth removeVethFunc, listTaps listTapsFunc) (*IMDSServiceImpl, error) {
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
	if listTaps == nil {
		return nil, errors.New("nil tap enumerator")
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
		tokens:   newTokenStore(),
		creds:    newCredCache(sts),
		iam:      iamSvc,
		pubKeys:  pubKeys,
		listTaps: listTaps,
		now:      time.Now,
	}
	// One shared mux serves both stacks: the per-subnet localport listeners and
	// the per-tap responders thread their respective identities via BaseContext.
	handler := svc.httpHandler()
	svc.bind = newBindManager(vethKV, handler, ensureVeth, removeVeth, bindLocalListener)
	svc.tapResp = newTapResponderManager(handler, svc.resolver.resolveENIByID, bindTapListener)

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
	return rejectForwarded(normalizeVersion(mux))
}

// Run starts the bind manager (sync + watch), the per-tap reconcile loop, and the
// token-sweep ticker, then blocks until ctx is done. Initial failures are
// non-fatal; vpcd readiness must not be gated on IMDS.
func (s *IMDSServiceImpl) Run(ctx context.Context) error {
	if err := s.bind.sync(ctx); err != nil {
		slog.Warn("IMDS: initial bind sync failed, continuing", "err", err)
	}
	go s.bind.watch(ctx)
	go s.reconcileTaps(ctx)
	go s.sweepExpired(ctx)

	<-ctx.Done()
	s.bind.shutdown()
	s.tapResp.shutdown()
	return ctx.Err()
}

// reconcileTaps reconciles the per-tap responders against the live OVS tap set on
// a ticker (plus once at start), so a freshly launched guest's responder begins
// serving within one interval and a vpcd restart re-binds every existing tap.
// This is the serving owner: the daemon installs the host datapath at SetupTap,
// but the responder lives here in vpcd and binds on the next reconcile.
func (s *IMDSServiceImpl) reconcileTaps(ctx context.Context) {
	s.reconcileTapsOnce(ctx)
	ticker := time.NewTicker(tapReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileTapsOnce(ctx)
		}
	}
}

// reconcileTapsOnce enumerates the local IMDS taps and converges the responders
// to them. A list error is logged and skipped so the next tick retries.
func (s *IMDSServiceImpl) reconcileTapsOnce(ctx context.Context) {
	live, err := s.listTaps(ctx)
	if err != nil {
		slog.Warn("IMDS: list taps during reconcile failed, retrying next tick", "err", err)
		return
	}
	s.tapResp.reconcile(ctx, live)
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
