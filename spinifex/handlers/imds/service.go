package handlers_imds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/service/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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
	ResolveInstanceProfile(ctx context.Context, accountID, nameOrARN string) (*handlers_iam.InstanceProfile, error)
	GetRole(ctx context.Context, accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error)
}

// publicKeyLookup fetches the SSH public key material for the public-keys path.
type publicKeyLookup interface {
	GetPublicKey(ctx context.Context, accountID, keyName string) (string, error)
}

// IMDSService is the host-served EC2 Instance Metadata Service (169.254.169.254).
type IMDSService interface {
	Run(ctx context.Context) error
}

var _ IMDSService = (*IMDSServiceImpl)(nil)

// eniResolver maps a tap's ENI ID to ENI + instance facts.
type eniResolver interface {
	resolveENIByID(ctx context.Context, eniID string) (*eniFacts, error)
	resolveInstance(ctx context.Context, eni *eniFacts) (*instanceFacts, error)
	resolveSGNames(ctx context.Context, accountID string, sgIDs []string) []string
	resolveSubnetCIDR(ctx context.Context, accountID, subnetID string) (string, error)
	resolveVPCCIDR(ctx context.Context, accountID, vpcID string) (string, error)
}

// IMDSServiceImpl is the in-process IMDS implementation. It runs one per-tap
// responder per local primary-ENI tap, each serving the shared mux with the
// tap's ENI identity.
type IMDSServiceImpl struct {
	resolver       eniResolver
	tokens         *tokenStore
	creds          *credCache
	iam            profileLookup
	pubKeys        publicKeyLookup
	tapResp        *tapResponderManager
	listTaps       listTapsFunc
	now            func() time.Time
	baseDomain     string
	internalDomain string
}

// NewIMDSServiceImpl wires the IMDS service. listTaps is injected to avoid a
// network/host import cycle. baseDomain and internalDomain are the cluster's
// authoritative public and private (AWS-parity) DNS domains, used for
// public-hostname and local-hostname so IMDS matches the records the DNS writer
// publishes. resolverIPs are the WAN IPs of nodes running northstar: when
// non-empty, each per-tap responder also serves the VPC DNS shim on
// 169.254.169.253:53, relaying to northstar's unprivileged wildcard listener.
// ctx bounds the bucket opens only; each served request carries its own.
func NewIMDSServiceImpl(ctx context.Context, natsConn *nats.Conn, sts stsAssumer, iamSvc profileLookup, pubKeys publicKeyLookup, expectedNodes int, listTaps listTapsFunc, baseDomain, internalDomain string, resolverIPs []string) (*IMDSServiceImpl, error) {
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
	if listTaps == nil {
		return nil, errors.New("nil tap enumerator")
	}

	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, fmt.Errorf("get JetStream context: %w", err)
	}

	eniKV, err := js.KeyValue(ctx, kvBucketENIs)
	if err != nil {
		return nil, fmt.Errorf("open %s bucket: %w", kvBucketENIs, err)
	}

	// Open SG bucket best-effort; IMDS starts fine without it, degrading to raw IDs.
	sgKV, err := js.KeyValue(ctx, kvBucketSecurityGroups)
	if err != nil {
		slog.Warn("IMDS: security-group bucket unavailable, /security-groups will serve IDs", "bucket", kvBucketSecurityGroups, "err", err)
		sgKV = nil
	}

	// Open subnet/VPC buckets best-effort; the network-interfaces CIDR leaves 404
	// (and drop from the listing) when a bucket is unavailable, IMDS still starts.
	subnetKV, err := js.KeyValue(ctx, kvBucketSubnets)
	if err != nil {
		slog.Warn("IMDS: subnet bucket unavailable, network-interfaces subnet CIDR will 404", "bucket", kvBucketSubnets, "err", err)
		subnetKV = nil
	}
	vpcKV, err := js.KeyValue(ctx, kvBucketVPCs)
	if err != nil {
		slog.Warn("IMDS: vpc bucket unavailable, network-interfaces VPC CIDR will 404", "bucket", kvBucketVPCs, "err", err)
		vpcKV = nil
	}

	svc := &IMDSServiceImpl{
		resolver: &metadataResolver{
			eniKV:    eniKV,
			sgKV:     sgKV,
			subnetKV: subnetKV,
			vpcKV:    vpcKV,
			lookup:   &natsInstanceLookup{nc: natsConn, expectedNodes: expectedNodes},
		},
		tokens:         newTokenStore(),
		creds:          newCredCache(sts),
		iam:            iamSvc,
		pubKeys:        pubKeys,
		listTaps:       listTaps,
		now:            time.Now,
		baseDomain:     baseDomain,
		internalDomain: internalDomain,
	}
	// Each per-tap responder serves the shared mux, threading its tap's ENI
	// identity into every request via BaseContext.
	svc.tapResp = newTapResponderManager(svc.httpHandler(), svc.resolver.resolveENIByID, bindTapListener)
	if len(resolverIPs) > 0 {
		targets := make([]string, 0, len(resolverIPs))
		for _, ip := range resolverIPs {
			targets = append(targets, net.JoinHostPort(ip, northstarResolverPort))
		}
		svc.tapResp.enableDNS(bindTapDNS, newDNSForwarder(targets))
		slog.Info("IMDS: VPC DNS shim enabled", "addr", VPCDNSServerIP, "backends", targets)
	}

	slog.Info("IMDS service initialized", "eni_bucket", kvBucketENIs)
	return svc, nil
}

// httpHandler builds the shared mux for every per-tap responder.
func (s *IMDSServiceImpl) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pathToken, s.handleToken)
	mux.HandleFunc("/", s.handleMetadata)
	return rejectForwarded(normalizeVersion(mux))
}

// Run starts the per-tap reconcile loop and the token-sweep ticker, then blocks
// until ctx is done. Initial failures are non-fatal; vpcd readiness must not be
// gated on IMDS.
func (s *IMDSServiceImpl) Run(ctx context.Context) error {
	go s.reconcileTaps(ctx)
	go s.sweepExpired(ctx)

	<-ctx.Done()
	s.tapResp.shutdown()
	return ctx.Err()
}

// reconcileTaps reconciles the per-tap responders against the live OVS tap set on a
// ticker (plus once at start), so a freshly launched guest serves within one interval
// and a vpcd restart re-binds every tap. The daemon installs the datapath; this serves.
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
		slog.WarnContext(ctx, "IMDS: list taps during reconcile failed, retrying next tick", "err", err)
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
