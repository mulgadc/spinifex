// Package ocibsd is an EBSProvider that stores every volume as an OCI Block
// Volume, every snapshot as an incremental OCI volume backup, and attaches a
// published volume to its node over iSCSI.
//
// A Provider runs on one node and answers for it alone: PublishVolume and
// UnpublishVolume act on that node's instance. Every other verb can be
// answered by any node, because OCI holds all of the state.
package ocibsd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
)

// Freeform tags ocibsd sets on the OCI resources it creates. They carry a
// resource's EBS identity and requested size, so no other store is needed.
const (
	TagCluster       = "spinifex-cluster"
	TagVolumeID      = "spinifex-volume-id"
	TagSnapshotID    = "spinifex-snapshot-id"
	TagCapacityBytes = "spinifex-capacity-bytes"
)

const gib = int64(1) << 30

// VPUs per GB for OCI's Balanced and Higher Performance tiers.
const (
	BalancedVPUsPerGB          int64 = 10
	HigherPerformanceVPUsPerGB int64 = 20
)

const (
	defaultPollInterval   = 2 * time.Second
	defaultVolumeTimeout  = 5 * time.Minute
	defaultAttachTimeout  = 3 * time.Minute
	defaultDetachTimeout  = 3 * time.Minute
	retryTokenHashLetters = 48

	// defaultPublishTimeout bounds a whole publish short of the client's
	// budget, so a slow attach is answered as retry-later rather than abandoned.
	defaultPublishTimeout = ebsprovider.PublishTimeout - 30*time.Second
)

// Config is what a Provider needs to reach OCI and to act for its node.
type Config struct {
	// ClusterID tags every resource, so one compartment can hold several
	// clusters without one enumerating or deleting another's.
	ClusterID     string
	CompartmentID string

	// AvailabilityDomain is where every volume is created, and
	// AvailabilityZone is the EC2 name of the cluster's one zone.
	AvailabilityDomain string
	AvailabilityZone   string

	// NodeID, InstanceID and InstanceAvailabilityDomain describe the node this
	// provider runs on; the last comes from the instance metadata service.
	NodeID                     string
	InstanceID                 string
	InstanceAvailabilityDomain string

	Client    oci.BlockClient
	Initiator Initiator

	// Zero values take the defaults above.
	PollInterval  time.Duration
	VolumeTimeout time.Duration
	AttachTimeout time.Duration
	DetachTimeout time.Duration
	// PublishTimeout bounds a whole publish, attach and login included.
	PublishTimeout time.Duration
}

// Provider is the ocibsd EBSProvider for one node.
type Provider struct {
	cfg Config

	// started is set once Start has logged out stale sessions.
	started atomic.Bool

	mu    sync.Mutex
	locks map[string]*volumeLock
}

type volumeLock struct {
	mu      sync.Mutex
	holders int
}

var _ ebsprovider.EBSProvider = (*Provider)(nil)

// New returns a Provider for cfg, refusing a node outside the cluster's AD:
// a volume created there could never be attached to it.
func New(cfg Config) (*Provider, error) {
	for name, value := range map[string]string{
		"cluster ID": cfg.ClusterID, "compartment ID": cfg.CompartmentID,
		"availability domain": cfg.AvailabilityDomain, "availability zone": cfg.AvailabilityZone,
		"node ID": cfg.NodeID, "instance ID": cfg.InstanceID,
	} {
		if value == "" {
			return nil, fmt.Errorf("ocibsd: %s is required", name)
		}
	}
	if cfg.Client == nil || cfg.Initiator == nil {
		return nil, errors.New("ocibsd: an OCI block client and an iSCSI initiator are required")
	}
	if cfg.InstanceAvailabilityDomain != cfg.AvailabilityDomain {
		return nil, fmt.Errorf("ocibsd: instance %s is in availability domain %q, but the cluster's volumes are in %q",
			cfg.InstanceID, cfg.InstanceAvailabilityDomain, cfg.AvailabilityDomain)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.VolumeTimeout <= 0 {
		cfg.VolumeTimeout = defaultVolumeTimeout
	}
	if cfg.AttachTimeout <= 0 {
		cfg.AttachTimeout = defaultAttachTimeout
	}
	if cfg.DetachTimeout <= 0 {
		cfg.DetachTimeout = defaultDetachTimeout
	}
	if cfg.PublishTimeout <= 0 {
		cfg.PublishTimeout = defaultPublishTimeout
	}
	return &Provider{cfg: cfg, locks: map[string]*volumeLock{}}, nil
}

// Capabilities is what ocibsd advertises. OCI refuses a second attachment of
// a volume that is not shareable wherever it is asked for, so exclusion is
// cluster-wide; an attachment never lapses, and a cut session is not fenced.
func Capabilities() ebsprovider.Capabilities {
	return ebsprovider.Capabilities{
		CrashConsistentSnapshot: true,
		ReadOnlyPublish:         true,
		VolumeEnumeration:       true,
		SnapshotEnumeration:     true,
		Exclusion: ebsprovider.ExclusionSemantics{
			Scope:           ebsprovider.ExclusionScopeCluster,
			ClaimTTLSeconds: 0,
			FencesLostClaim: false,
		},
	}
}

func (p *Provider) GetCapabilities(_ context.Context, req ebsprovider.GetCapabilitiesRequest) (*ebsprovider.GetCapabilitiesResponse, error) {
	if err := ebsprovider.CheckVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	return &ebsprovider.GetCapabilitiesResponse{
		Versioned:        ebsprovider.ReplyVersioned(req.SchemaVersion),
		Capabilities:     Capabilities(),
		ContractVersions: ebsprovider.SupportedSchemaVersions(),
	}, nil
}

// HoldsUnsealedWrites answers that none are held: QEMU opens the device with
// cache.direct=on, and the iSCSI target acknowledges a write once OCI has it.
func (p *Provider) HoldsUnsealedWrites(_ context.Context, nodeID string, volumeIDs []string) ([]string, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("%w: node ID is required", ebsprovider.ErrInvalidArgument)
	}
	if slices.Contains(volumeIDs, "") {
		return nil, fmt.Errorf("%w: empty volume ID", ebsprovider.ErrInvalidArgument)
	}
	return nil, nil
}

// RetryToken is the opc-retry-token for creating the resource kind named id
// in cluster, so a create repeated after a lost reply makes nothing new.
func RetryToken(kind, cluster, id string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + cluster + "\x00" + id))
	return "spx-" + hex.EncodeToString(sum[:])[:retryTokenHashLetters]
}

// SizeGB is the OCI size of a volume of capacityBytes: whole GB rounded up,
// and never below OCI's floor. OCI's GB is taken to be 2^30 bytes.
func SizeGB(capacityBytes int64) int64 {
	return max((capacityBytes+gib-1)/gib, oci.MinVolumeSizeGB)
}

// VPUsPerGB maps an EBS volume type to an OCI performance tier. The
// provisioned-IOPS types get Higher Performance and every other type Balanced.
func VPUsPerGB(volumeType string) (int64, error) {
	switch volumeType {
	case "", "gp2", "gp3", "st1", "sc1":
		return BalancedVPUsPerGB, nil
	case "io1", "io2":
		return HigherPerformanceVPUsPerGB, nil
	default:
		return 0, fmt.Errorf("%w: volume type %q has no OCI performance tier", ebsprovider.ErrInvalidArgument, volumeType)
	}
}

// capacityOf reads the recorded EBS size from a resource's tags.
func capacityOf(tags map[string]string) (int64, error) {
	raw, ok := tags[TagCapacityBytes]
	if !ok {
		return 0, fmt.Errorf("ocibsd: resource has no %s tag", TagCapacityBytes)
	}
	capacity, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || capacity <= 0 {
		return 0, fmt.Errorf("ocibsd: resource has an invalid %s tag %q", TagCapacityBytes, raw)
	}
	return capacity, nil
}

// ociError translates an OCI failure no verb handles itself. A conflict takes
// the onConflict sentinel when the verb gives one, a quota or throttling
// refusal is retry-later, and anything else is passed on as is.
func ociError(op string, err error, onConflict ...error) error {
	if len(onConflict) > 0 && errors.Is(err, oci.ErrConflict) {
		return fmt.Errorf("%w: %s: %w", onConflict[0], op, err)
	}
	if errors.Is(err, oci.ErrLimitExceeded) {
		// The contract has no resource-exhausted code, and the same request
		// succeeds once the limit frees, as when another volume detaches.
		return fmt.Errorf("%w: %s: %w", ebsprovider.ErrUnavailable, op, err)
	}
	return fmt.Errorf("ocibsd: %s: %w", op, err)
}

// errGaveUp is what poll returns once its timeout passes.
var errGaveUp = errors.New("ocibsd: gave up waiting")

// poll calls done every interval until it reports true or fails, returning
// errGaveUp once timeout passes and ctx's error if ctx ends first.
func poll(ctx context.Context, timeout, interval time.Duration, what string, done func(context.Context) (bool, error)) error {
	start := time.Now()
	for {
		ok, err := done(ctx)
		if err != nil || ok {
			return err
		}
		if time.Since(start) >= timeout {
			slog.WarnContext(ctx, "ocibsd: gave up waiting", "what", what, "waited_ms", otelsetup.Millis(time.Since(start)))
			return errGaveUp
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// waitFor polls an OCI operation, failing as retry-later once timeout passes,
// since the same request may then find the work finished.
func (p *Provider) waitFor(ctx context.Context, timeout time.Duration, what string, done func(context.Context) (bool, error)) error {
	err := poll(ctx, timeout, p.cfg.PollInterval, what, done)
	if errors.Is(err, errGaveUp) {
		return fmt.Errorf("%w: %s did not finish in time", ebsprovider.ErrUnavailable, what)
	}
	return err
}

// lockVolume serialises this node's publish and unpublish of one volume, so
// two concurrent publishes here do not both attach it.
func (p *Provider) lockVolume(volumeID string) func() {
	p.mu.Lock()
	lock, ok := p.locks[volumeID]
	if !ok {
		lock = &volumeLock{}
		p.locks[volumeID] = lock
	}
	lock.holders++
	p.mu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		p.mu.Lock()
		if lock.holders--; lock.holders == 0 {
			delete(p.locks, volumeID)
		}
		p.mu.Unlock()
	}
}
