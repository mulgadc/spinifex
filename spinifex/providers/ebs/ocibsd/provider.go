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
	if err := checkVersion(req.SchemaVersion); err != nil {
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

func checkVersion(version uint16) error {
	if !ebsprovider.SupportsSchemaVersion(version) {
		return fmt.Errorf("%w: got %d, %s", ebsprovider.ErrUnsupportedVersion, version, ebsprovider.SupportedVersionsText())
	}
	return nil
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

// ociError translates an OCI failure no verb handles itself. A quota or
// throttling refusal is retry-later; anything else is passed on as is.
func ociError(op string, err error) error {
	if errors.Is(err, oci.ErrLimitExceeded) {
		return fmt.Errorf("%w: %s: %w", ebsprovider.ErrUnavailable, op, err)
	}
	return fmt.Errorf("ocibsd: %s: %w", op, err)
}

// waitFor polls done until it reports true, failing as retry-later once
// timeout passes, since the same request may then find the work finished.
func (p *Provider) waitFor(ctx context.Context, timeout time.Duration, what string, done func(context.Context) (bool, error)) error {
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		ok, err := done(ctx)
		if err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			slog.WarnContext(ctx, "ocibsd: gave up waiting", "what", what, "waited_ms", otelsetup.Millis(time.Since(start)))
			return fmt.Errorf("%w: %s did not finish in time", ebsprovider.ErrUnavailable, what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.cfg.PollInterval):
		}
	}
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
