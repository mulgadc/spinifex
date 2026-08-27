package handlers_ec2_snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/bluebottle/pkg/safecast"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/handlers/ec2/volumestate"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Ensure SnapshotServiceImpl implements SnapshotService.
var _ SnapshotService = (*SnapshotServiceImpl)(nil)

const (
	KVBucketVolumeSnapshots        = "spinifex-volume-snapshots"
	KVBucketVolumeSnapshotsVersion = 1
	snapshotCleanupTimeout         = 5 * time.Second
)

// SnapshotServiceImpl implements SnapshotService with S3-backed storage.
type SnapshotServiceImpl struct {
	config   *config.Config
	store    objectstore.ObjectStore
	natsConn *nats.Conn
	snapKV   jetstream.KeyValue
	mutex    sync.RWMutex
	metadata *ebsmetadata.Store
	provider ebsprovider.EBSProvider
}

// SetEBSProvider injects the provider boundary for snapshot operations.
// Legacy snapshot behavior remains until snapshot metadata is migrated.
func (s *SnapshotServiceImpl) SetEBSProvider(provider ebsprovider.EBSProvider) {
	s.provider = provider
}

// EBSProvider returns the injected provider boundary, or nil on the legacy
// embedded-engine path. Primarily for composition-root tests to observe wiring.
func (s *SnapshotServiceImpl) EBSProvider() ebsprovider.EBSProvider {
	return s.provider
}

// MetadataStore returns the control-plane metadata store. Primarily for
// composition-root tests to observe wiring.
func (s *SnapshotServiceImpl) MetadataStore() *ebsmetadata.Store {
	return s.metadata
}

// SnapshotConfig represents snapshot metadata stored in S3.
type SnapshotConfig struct {
	SnapshotID       string            `json:"snapshot_id"`
	VolumeID         string            `json:"volume_id"`
	VolumeSize       int64             `json:"volume_size"`
	State            string            `json:"state"`
	Progress         string            `json:"progress"`
	StartTime        time.Time         `json:"start_time"`
	Description      string            `json:"description"`
	Encrypted        bool              `json:"encrypted"`
	OwnerID          string            `json:"owner_id"`
	AvailabilityZone string            `json:"availability_zone"`
	Tags             map[string]string `json:"tags"`
	ProviderHandle   string            `json:"provider_handle,omitempty"`
}

// NewSnapshotServiceImplWithNATS creates a snapshot service with JetStream KV for volume-snapshot tracking.
func NewSnapshotServiceImplWithNATS(ctx context.Context, cfg *config.Config, natsConn *nats.Conn) (*SnapshotServiceImpl, jetstream.KeyValue, error) {
	store := objectstore.NewS3ObjectStoreFromConfig(
		cfg.Predastore.Host,
		cfg.Predastore.Region,
		cfg.Predastore.AccessKey,
		cfg.Predastore.SecretKey,
	)

	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, nil, fmt.Errorf("create JetStream: %w", err)
	}

	kv, err := kvutil.GetOrCreateBucket(ctx, js, KVBucketVolumeSnapshots, 10)
	if err != nil {
		return nil, nil, fmt.Errorf("create KV bucket %s: %w", KVBucketVolumeSnapshots, err)
	}
	if err := migrate.DefaultRegistry.RunKV(ctx, KVBucketVolumeSnapshots, kv, KVBucketVolumeSnapshotsVersion); err != nil {
		return nil, nil, fmt.Errorf("migrate %s: %w", KVBucketVolumeSnapshots, err)
	}

	slog.Info("Snapshot service initialized with JetStream KV", "bucket", KVBucketVolumeSnapshots)

	return &SnapshotServiceImpl{
		config:   cfg,
		store:    store,
		natsConn: natsConn,
		snapKV:   kv,
		metadata: ebsmetadata.NewStore(store, cfg.Predastore.Bucket),
	}, kv, nil
}

// NewSnapshotServiceImplWithStore creates a snapshot service with a custom ObjectStore (for testing).
// An optional snapshotKV can be provided for KV-backed volume-snapshot tracking.
func NewSnapshotServiceImplWithStore(cfg *config.Config, store objectstore.ObjectStore, natsConn *nats.Conn, snapshotKV ...jetstream.KeyValue) *SnapshotServiceImpl {
	svc := &SnapshotServiceImpl{
		config:   cfg,
		store:    store,
		natsConn: natsConn,
		metadata: ebsmetadata.NewStore(store, cfg.Predastore.Bucket),
	}
	if len(snapshotKV) > 0 {
		svc.snapKV = snapshotKV[0]
	}
	return svc
}

// GetSnapshotKey uses metadata.json to avoid colliding with viperblock's
// config.json (which stores SnapshotState: block map, source volume, etc).
func GetSnapshotKey(snapshotID string) string {
	return fmt.Sprintf("%s/metadata.json", snapshotID)
}

// ErrCorruptSnapshotMetadata lets callers distinguish a missing snapshot from
// one whose metadata.json can't be parsed.
var ErrCorruptSnapshotMetadata = errors.New("corrupt snapshot metadata")

// ReadSnapshotConfig reads {snapshotID}/metadata.json. Object-store errors are
// returned unchanged; callers map NoSuchKey to their preferred AWS error.
// Decode failures wrap ErrCorruptSnapshotMetadata. ctx carries the caller's
// deadline: a describe that has already given up must not keep the object
// store busy on its behalf.
func ReadSnapshotConfig(ctx context.Context, store objectstore.ObjectStore, bucket, snapshotID string) (*SnapshotConfig, error) {
	key := GetSnapshotKey(snapshotID)
	result, err := store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()

	var cfg SnapshotConfig
	if err := json.NewDecoder(result.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptSnapshotMetadata, err)
	}
	return &cfg, nil
}

// WriteSnapshotConfig writes the SnapshotConfig to {snapshotID}/metadata.json.
func WriteSnapshotConfig(store objectstore.ObjectStore, bucket, snapshotID string, cfg *SnapshotConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(GetSnapshotKey(snapshotID)),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}

// getSnapshotConfig translates NoSuchKey to InvalidSnapshot.NotFound.
func (s *SnapshotServiceImpl) getSnapshotConfig(ctx context.Context, snapshotID string) (*SnapshotConfig, error) {
	cfg, err := ReadSnapshotConfig(ctx, s.store, s.config.Predastore.Bucket, snapshotID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
		}
		return nil, err
	}
	return cfg, nil
}

// putSnapshotConfig stores snapshot config to S3.
func (s *SnapshotServiceImpl) putSnapshotConfig(snapshotID string, cfg *SnapshotConfig) error {
	return WriteSnapshotConfig(s.store, s.config.Predastore.Bucket, snapshotID, cfg)
}

// snapshotConfigToEC2 converts a SnapshotConfig to an EC2 Snapshot response object.
func snapshotConfigToEC2(cfg *SnapshotConfig) *ec2.Snapshot {
	snapshot := &ec2.Snapshot{
		SnapshotId:  aws.String(cfg.SnapshotID),
		VolumeId:    aws.String(cfg.VolumeID),
		VolumeSize:  aws.Int64(cfg.VolumeSize),
		State:       aws.String(cfg.State),
		Progress:    aws.String(cfg.Progress),
		StartTime:   aws.Time(cfg.StartTime),
		Description: aws.String(cfg.Description),
		Encrypted:   aws.Bool(cfg.Encrypted),
		OwnerId:     aws.String(cfg.OwnerID),
	}

	snapshot.Tags = utils.MapToEC2Tags(cfg.Tags)

	return snapshot
}

// CreateSnapshot creates a new snapshot from a volume.
func (s *SnapshotServiceImpl) CreateSnapshot(ctx context.Context, input *ec2.CreateSnapshotInput, accountID string) (*ec2.Snapshot, error) {
	if input == nil || input.VolumeId == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	volumeID := *input.VolumeId

	slog.InfoContext(ctx, "CreateSnapshot request", "volumeId", volumeID)

	snapshotID := utils.GenerateResourceID("snap")
	if s.provider == nil {
		slog.ErrorContext(ctx, "no EBS provider configured", "op", "CreateSnapshot")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// A volume predating tenancy records no owner. Refusing it would strand
	// every pre-tenancy volume as unsnapshottable, so an absent TenantID stays
	// open to any caller; a recorded one must match.
	volume, err := s.metadata.GetVolume(ctx, volumeID)
	if err != nil || (accountID != "" && volume.TenantID != "" && volume.TenantID != accountID) {
		return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}
	if volume.CapacityGiB == 0 {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Flush the writes buffered by whichever node serves the volume, so the
	// provider reads a current checkpoint. An attached volume that cannot be
	// drained fails here rather than silently snapshotting stale data.
	if err := s.drainVolume(ctx, volumeID, volume.State, volume.AttachedInstance, accountID); err != nil {
		slog.ErrorContext(ctx, "CreateSnapshot: drain failed", "volumeId", volumeID, "snapshotId", snapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	created, err := s.provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, VolumeID: volumeID, VolumeHandle: volume.ProviderHandle,
	})
	if err != nil || created == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	now := time.Now()
	snapshotCfg := &SnapshotConfig{
		SnapshotID: snapshotID, VolumeID: volumeID,
		VolumeSize: safecast.Uint64ToInt64(volume.CapacityGiB),
		State:      string(created.State), Progress: "100%", StartTime: now,
		Encrypted: volume.Encrypted,
		OwnerID:   accountID, AvailabilityZone: volume.AvailabilityZone,
		Tags:           utils.ExtractTags(input.TagSpecifications, "snapshot"),
		ProviderHandle: created.Handle,
	}
	if snapshotCfg.State == "" {
		snapshotCfg.State = "completed"
	}
	if input.Description != nil {
		snapshotCfg.Description = *input.Description
	}

	// Track the volume→snapshot dependency in KV before persisting to S3.
	// This ensures we never have an untracked snapshot in S3.
	if err := s.addSnapshotRef(ctx, volumeID, snapshotID); err != nil {
		slog.ErrorContext(ctx, "CreateSnapshot failed to add snapshot ref to KV", "snapshotId", snapshotID, "volumeId", volumeID, "err", err)
		_ = s.provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, Handle: created.Handle})
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if err := s.putSnapshotConfig(snapshotID, snapshotCfg); err != nil {
		slog.ErrorContext(ctx, "CreateSnapshot failed to write config", "snapshotId", snapshotID, "err", err)
		_ = s.removeSnapshotRefForCleanup(ctx, volumeID, snapshotID)
		_ = s.provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, Handle: created.Handle})
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "CreateSnapshot completed", "snapshotId", snapshotID, "volumeId", volumeID)

	return snapshotConfigToEC2(snapshotCfg), nil
}

const (
	// drainDialTimeout bounds the connect to the local drain socket. The socket
	// is either being served on this node or it is not, so a slow connect means
	// a wedged plugin rather than a busy one.
	drainDialTimeout = time.Second

	// drainAckTimeout bounds DrainToBackend on the hosting node: it flushes the
	// WAL and the live checkpoint to S3, so it scales with the dirty set rather
	// than with a fixed unit of work.
	drainAckTimeout = 30 * time.Second

	// drainRequestTimeout bounds the ec2.cmd round-trip. It exceeds
	// drainAckTimeout so a slow drain surfaces as the hosting node's error
	// instead of an opaque NATS timeout here.
	drainRequestTimeout = 35 * time.Second
)

// ErrNoDrainSocket reports that this node has no drain socket for the volume,
// which is how a node learns it does not serve it. It is deliberately distinct
// from a socket that answered badly: only the former is worth routing onward,
// and conflating the two makes a node re-run a drain that has already failed.
var ErrNoDrainSocket = errors.New("no drain socket on this node")

// DrainVolumeSocket asks the NBD plugin serving volumeID on this node to flush
// its WAL chunks and live checkpoint to S3, then waits for the ack. The plugin
// creates the socket under {dataDir}/viperblock/{volumeID}/, so this only
// succeeds on the node hosting the volume; the daemon calls it when a drain
// command is routed to it.
func DrainVolumeSocket(dataDir, volumeID string) error {
	sockPath := filepath.Join(dataDir, "viperblock", volumeID, "snapshot.sock")
	conn, err := net.DialTimeout("unix", sockPath, drainDialTimeout)
	if err != nil {
		return fmt.Errorf("%w: dial %s: %w", ErrNoDrainSocket, sockPath, err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(drainAckTimeout))
	buf := make([]byte, 8)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("read drain ack for %s: %w", volumeID, err)
	}
	if !strings.HasPrefix(string(buf[:n]), "OK") {
		return fmt.Errorf("drain of %s did not ack OK: %q", volumeID, string(buf[:n]))
	}
	return nil
}

// drainVolume flushes the volume's in-flight writes to S3 before the snapshot
// reads the live checkpoint from there.
func (s *SnapshotServiceImpl) drainVolume(ctx context.Context, volumeID string, fallbackState, fallbackInstance string, accountID string) error {
	return DrainVolume(ctx, s.config, s.store, s.natsConn, volumeID, fallbackState, fallbackInstance, accountID)
}

// DrainVolume flushes the volume's in-flight writes to S3 before a live
// checkpoint is read from there. Shared by ec2.CreateSnapshot and
// ec2.CreateImage's running-instance path (handlers/ec2/image) so the two
// live-snapshot call sites cannot diverge on when a drain is required.
//
// The drain socket lives on the node serving the volume, but both callers are
// queue-grouped across every node, so the node answering the request usually
// is not that node. Whether a drain is required is therefore decided from the
// volume's attachment record, never from whether the socket happens to be
// local: an attached volume is being written to right now, and reading its
// checkpoint without draining silently captures an older one.
func DrainVolume(ctx context.Context, cfg *config.Config, store objectstore.ObjectStore, natsConn *nats.Conn, volumeID string, fallbackState, fallbackInstance string, accountID string) error {
	// A metadata-only snapshot never reads the live checkpoint, so there is
	// nothing for a drain to make current.
	if cfg == nil || cfg.Predastore.Host == "" {
		return nil
	}

	state, instanceID, err := volumeAttachment(ctx, store, cfg.Predastore.Bucket, volumeID, fallbackState, fallbackInstance)
	if err != nil {
		return err
	}

	// Nothing is writing to an unattached volume: the checkpoint its Close()
	// left behind is the current one. Decide this before touching the socket —
	// dialing it is not a probe, it makes the plugin run a full flush.
	if state != "in-use" || instanceID == "" {
		slog.InfoContext(ctx, "DrainVolume: volume not attached, snapshotting the checkpoint left by Close (stopped instance path)",
			"volumeId", volumeID, "state", state, "attachedInstance", instanceID)
		return nil
	}

	// Fast path: the volume is served by this node, so no hop is needed. This is
	// every single-node deployment, and the node hosting the instance on a
	// cluster.
	localErr := DrainVolumeSocket(cfg.DataDir, volumeID)
	if localErr == nil {
		return nil
	}

	// A socket that answered but did not ack means this node does serve the
	// volume and the flush itself failed. Routing that onward would land back on
	// this same node and repeat the drain that has already failed.
	if !errors.Is(localErr, ErrNoDrainSocket) {
		return fmt.Errorf("drain volume %s on this node: %w", volumeID, localErr)
	}

	slog.InfoContext(ctx, "DrainVolume: volume attached, routing drain to the node hosting it",
		"volumeId", volumeID, "instanceId", instanceID)
	return drainOnHostNode(ctx, natsConn, volumeID, instanceID, accountID)
}

// volumeAttachment returns the volume's authoritative state and attached
// instance. state.json is control-plane-owned and authoritative; the fallback
// pair (passed in from the caller's own volume record) is rewritten by the
// live NBD plugin from its stale in-memory state and is only used for volumes
// predating the split.
func volumeAttachment(ctx context.Context, store objectstore.ObjectStore, bucket, volumeID string, fallbackState, fallbackInstance string) (state, instanceID string, err error) {
	rec, found, err := volumestate.Read(ctx, store, bucket, volumeID)
	if err != nil {
		return "", "", fmt.Errorf("read volume state for %s: %w", volumeID, err)
	}
	if found {
		return rec.State, rec.AttachedInstance, nil
	}
	return fallbackState, fallbackInstance, nil
}

// drainOnHostNode issues the drain on the node hosting instanceID. Only that
// node subscribes ec2.cmd.{instanceID}, so the command is self-routing and no
// volume-to-node resolution is needed here.
//
// An attachment record is not proof of a live writer: stop deliberately leaves
// a boot volume attached (daemon/vm_adapters.go's volumeMounterAdapter.Unmount)
// while tearing down both the NBD plugin and this subscription. The two
// not-running signals below are therefore the stopped-instance path, not a
// failure — treating them as one would make every stopped instance's root
// volume permanently unsnapshottable.
func drainOnHostNode(ctx context.Context, natsConn *nats.Conn, volumeID, instanceID, accountID string) error {
	command := types.EC2InstanceCommand{
		ID:              instanceID,
		Attributes:      types.EC2CommandAttributes{DrainVolume: true},
		DrainVolumeData: &types.DrainVolumeData{VolumeID: volumeID},
	}

	resp, err := utils.NATSRequest[types.DrainVolumeResponse](ctx, natsConn,
		"ec2.cmd."+instanceID, command, drainRequestTimeout, accountID)
	if err != nil {
		// No subscriber at all: the instance runs nowhere in the cluster, so it
		// was stopped (stop migrates it to shared KV before unsubscribing) and
		// its volumes were sealed by Close. Warn rather than Info because the
		// same shape appears if the hosting node has lost NATS while its VM
		// keeps writing, which yields a stale snapshot as it did before routing
		// existed. A live host that answers and fails is still a hard error.
		if errors.Is(err, nats.ErrNoResponders) {
			slog.WarnContext(ctx, "drainVolume: no node hosts the instance, treating the volume as stopped and snapshotting its sealed checkpoint",
				"volumeId", volumeID, "instanceId", instanceID)
			return nil
		}
		return fmt.Errorf("drain volume %s on the node hosting %s: %w", volumeID, instanceID, err)
	}

	switch resp.Status {
	case types.DrainVolumeStatusDrained:
		return nil
	// The host still holds the instance but it is not running: nothing is
	// writing, so there is nothing to flush. This is a host-drain stop, which
	// keeps the VM (and this subscription) in place after unmounting.
	case types.DrainVolumeStatusNotRunning:
		slog.InfoContext(ctx, "drainVolume: instance is not running on its host, snapshotting the volume's sealed checkpoint",
			"volumeId", volumeID, "instanceId", instanceID)
		return nil
	default:
		return fmt.Errorf("drain volume %s on the node hosting %s: unexpected ack %q", volumeID, instanceID, resp.Status)
	}
}

// describeSnapshotsValidFilters defines the set of filter names accepted by DescribeSnapshots.
var describeSnapshotsValidFilters = map[string]bool{
	"snapshot-id": true,
	"status":      true,
	"volume-id":   true,
	"volume-size": true,
	"owner-id":    true,
}

// DescribeSnapshots lists snapshots matching the specified criteria, scoped to the caller's account.
func (s *SnapshotServiceImpl) DescribeSnapshots(ctx context.Context, input *ec2.DescribeSnapshotsInput, accountID string) (*ec2.DescribeSnapshotsOutput, error) {
	return s.describeSnapshots(ctx, input, accountID, false)
}

// DescribeSnapshotsStrict is the control-plane lookup contract. A metadata read
// failure makes the result non-authoritative, so it is returned rather than
// hidden as an absent snapshot.
func (s *SnapshotServiceImpl) DescribeSnapshotsStrict(ctx context.Context, input *ec2.DescribeSnapshotsInput, accountID string) (*ec2.DescribeSnapshotsOutput, error) {
	return s.describeSnapshots(ctx, input, accountID, true)
}

func (s *SnapshotServiceImpl) describeSnapshots(ctx context.Context, input *ec2.DescribeSnapshotsInput,
	accountID string, strict bool) (*ec2.DescribeSnapshotsOutput, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	slog.InfoContext(ctx, "DescribeSnapshots request", "snapshotIds", input.SnapshotIds, "accountID", accountID)

	snapshotIDFilter := make(map[string]bool)
	for _, id := range input.SnapshotIds {
		if id != nil {
			snapshotIDFilter[*id] = true
		}
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeSnapshotsValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeSnapshots: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	listResult, err := s.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.config.Predastore.Bucket),
		Prefix:    aws.String("snap-"),
		Delimiter: aws.String("/"),
	})
	if err != nil {
		slog.ErrorContext(ctx, "DescribeSnapshots failed to list objects", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Extract snapshot-id filter values for early prefix skipping to avoid
	// unnecessary S3 GetObject calls on non-matching snapshots.
	var snapshotIDFilterValues []string
	if parsedFilters != nil {
		snapshotIDFilterValues = parsedFilters["snapshot-id"]
	}

	var wanted []string
	for _, prefix := range listResult.CommonPrefixes {
		if prefix.Prefix == nil {
			continue
		}

		snapshotID := strings.TrimSuffix(*prefix.Prefix, "/")

		if len(snapshotIDFilter) > 0 && !snapshotIDFilter[snapshotID] {
			continue
		}

		// Early skip: if snapshot-id filter is set, check the prefix against
		// filter values before fetching config from S3.
		if len(snapshotIDFilterValues) > 0 {
			if !filterutil.MatchesAny(snapshotIDFilterValues, snapshotID) {
				continue
			}
		}

		wanted = append(wanted, snapshotID)
	}

	configs, readErrs := s.readSnapshotConfigs(ctx, wanted)

	// Every read fails once the caller's deadline passes, and reporting that as
	// an empty list would read as "this account has no snapshots".
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "DescribeSnapshots gave up reading metadata",
			"requested", len(wanted), "read", len(configs), "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var snapshots []*ec2.Snapshot
	for _, snapshotID := range wanted {
		cfg, ok := configs[snapshotID]
		if !ok {
			slog.WarnContext(ctx, "DescribeSnapshots failed to get config",
				"snapshotId", snapshotID, "err", readErrs[snapshotID])
			if strict {
				return nil, fmt.Errorf("describe snapshot %s metadata: %w", snapshotID, readErrs[snapshotID])
			}
			continue
		}

		// Filter by account: only return snapshots owned by the caller
		if accountID != "" && cfg.OwnerID != "" && cfg.OwnerID != accountID {
			continue
		}

		if len(parsedFilters) > 0 && !snapshotMatchesFilters(cfg, parsedFilters) {
			continue
		}

		snapshots = append(snapshots, snapshotConfigToEC2(cfg))
	}

	// Naming a specific, nonexistent snapshot ID is an error, unlike an
	// unfiltered list or a --filters query that simply matches nothing.
	if len(snapshotIDFilter) > 0 {
		found := make(map[string]bool, len(snapshots))
		for _, snap := range snapshots {
			if snap.SnapshotId != nil {
				found[*snap.SnapshotId] = true
			}
		}
		for id := range snapshotIDFilter {
			if !found[id] {
				return nil, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
			}
		}
	}

	slog.InfoContext(ctx, "DescribeSnapshots completed", "count", len(snapshots))

	return &ec2.DescribeSnapshotsOutput{
		Snapshots: snapshots,
	}, nil
}

// describeSnapshotFanout bounds how many metadata reads a single describe has
// in flight. A describe used to read every snapshot's metadata.json one after
// the other, so its latency was the sum of them all and a single slow object
// made the whole listing miss its caller's deadline. The bound keeps a large
// account from turning one describe into a burst against the object store.
const describeSnapshotFanout = 16

// readSnapshotConfigs fetches metadata for each snapshot concurrently. It
// returns what it could read plus the error for each one it could not, so the
// caller decides whether an unreadable snapshot is fatal or simply skipped.
func (s *SnapshotServiceImpl) readSnapshotConfigs(
	ctx context.Context, ids []string,
) (map[string]*SnapshotConfig, map[string]error) {
	configs := make(map[string]*SnapshotConfig, len(ids))
	readErrs := make(map[string]error)
	if len(ids) == 0 {
		return configs, readErrs
	}

	var mu sync.Mutex
	sem := make(chan struct{}, describeSnapshotFanout)
	var wg sync.WaitGroup
	for _, id := range ids {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			cfg, err := s.getSnapshotConfig(ctx, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				readErrs[id] = err
				return
			}
			configs[id] = cfg
		})
	}
	wg.Wait()
	return configs, readErrs
}

// snapshotMatchesFilters checks whether a SnapshotConfig satisfies all parsed filters.
func snapshotMatchesFilters(cfg *SnapshotConfig, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}

		var field string
		switch name {
		case "snapshot-id":
			field = cfg.SnapshotID
		case "status":
			field = cfg.State
		case "volume-id":
			field = cfg.VolumeID
		case "volume-size":
			field = strconv.FormatInt(cfg.VolumeSize, 10)
		case "owner-id":
			field = cfg.OwnerID
		default:
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	return filterutil.MatchesTags(filters, cfg.Tags)
}

// snapshotInUseByVolumes checks if any volume was created from the given
// snapshot. A clone records its source in its ebsmetadata document; missing one
// here would let a delete strip the chunks a live clone still reads.
func (s *SnapshotServiceImpl) snapshotInUseByVolumes(ctx context.Context, snapshotID string) (bool, error) {
	// Strict: a volume that failed to read is not evidence that no clone reads
	// the snapshot's chunks, and this answer gates deleting them.
	volumes, err := s.metadata.ListVolumesStrict(ctx)
	if err != nil {
		return false, fmt.Errorf("snapshotInUseByVolumes: failed to list volumes: %w", err)
	}
	for _, volume := range volumes {
		if volume.SnapshotID == snapshotID {
			return true, nil
		}
	}
	return false, nil
}

// DeleteSnapshot deletes a snapshot after verifying the caller owns it.
func (s *SnapshotServiceImpl) DeleteSnapshot(ctx context.Context, input *ec2.DeleteSnapshotInput, accountID string) (*ec2.DeleteSnapshotOutput, error) {
	if input == nil || input.SnapshotId == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	snapshotID := *input.SnapshotId

	slog.InfoContext(ctx, "DeleteSnapshot request", "snapshotId", snapshotID, "accountID", accountID)

	cfg, err := s.getSnapshotConfig(ctx, snapshotID)
	if err != nil {
		slog.ErrorContext(ctx, "DeleteSnapshot snapshot not found", "snapshotId", snapshotID, "err", err)
		return nil, err
	}

	// Verify ownership: caller must own the snapshot
	if accountID != "" && cfg.OwnerID != "" && cfg.OwnerID != accountID {
		slog.WarnContext(ctx, "DeleteSnapshot: account does not own snapshot", "snapshotId", snapshotID, "accountID", accountID, "ownerID", cfg.OwnerID)
		return nil, errors.New(awserrors.ErrorUnauthorizedOperation)
	}

	// Check if any volumes were created from this snapshot
	inUse, err := s.snapshotInUseByVolumes(ctx, snapshotID)
	if err != nil {
		slog.ErrorContext(ctx, "DeleteSnapshot failed to check snapshot usage", "snapshotId", snapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if inUse {
		slog.InfoContext(ctx, "DeleteSnapshot blocked: snapshot in use by volume", "snapshotId", snapshotID)
		return nil, errors.New(awserrors.ErrorInvalidSnapshotInUse)
	}
	if s.provider == nil {
		slog.ErrorContext(ctx, "no EBS provider configured", "op", "DeleteSnapshot")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if err := s.provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, Handle: cfg.ProviderHandle}); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	listResult, err := s.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.config.Predastore.Bucket),
		Prefix: aws.String(snapshotID + "/"),
	})
	if err != nil {
		slog.ErrorContext(ctx, "DeleteSnapshot failed to list objects", "snapshotId", snapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, obj := range listResult.Contents {
		if obj.Key == nil {
			continue
		}
		_, err := s.store.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.config.Predastore.Bucket),
			Key:    obj.Key,
		})
		if err != nil {
			slog.WarnContext(ctx, "DeleteSnapshot failed to delete object", "key", *obj.Key, "err", err)
		}
	}

	// Remove from KV after S3 cleanup. Failure is logged but not fatal —
	// a phantom entry safely blocks volume deletion rather than allowing it.
	if err := s.removeSnapshotRefForCleanup(ctx, cfg.VolumeID, snapshotID); err != nil {
		slog.Warn("DeleteSnapshot failed to remove snapshot ref from KV", "snapshotId", snapshotID, "volumeId", cfg.VolumeID, "err", err)
	}

	slog.InfoContext(ctx, "DeleteSnapshot completed", "snapshotId", snapshotID)

	return &ec2.DeleteSnapshotOutput{}, nil
}

// CopySnapshot duplicates a snapshot under a new ID owned by the caller.
// Served only through the provider: the copy needs its own snapshot metadata
// on the backend, and on an encrypted volume that metadata is AEAD-sealed with
// the snapshot ID in the AAD, so only the storage engine can re-seal it.
func (s *SnapshotServiceImpl) CopySnapshot(ctx context.Context, input *ec2.CopySnapshotInput, accountID string) (*ec2.CopySnapshotOutput, error) {
	if input == nil || input.SourceSnapshotId == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	sourceSnapshotID := *input.SourceSnapshotId

	slog.InfoContext(ctx, "CopySnapshot request", "sourceSnapshotId", sourceSnapshotID, "accountID", accountID)

	sourceCfg, err := s.getSnapshotConfig(ctx, sourceSnapshotID)
	if err != nil {
		slog.ErrorContext(ctx, "CopySnapshot source snapshot not found", "snapshotId", sourceSnapshotID, "err", err)
		return nil, err
	}

	if accountID != "" && sourceCfg.OwnerID != "" && sourceCfg.OwnerID != accountID {
		slog.WarnContext(ctx, "CopySnapshot: account does not own source snapshot", "snapshotId", sourceSnapshotID, "accountID", accountID, "ownerID", sourceCfg.OwnerID)
		return nil, errors.New(awserrors.ErrorUnauthorizedOperation)
	}

	// Without a provider the only reachable path writes the control-plane
	// config alone. That copy described and created volumes fine, then failed
	// at attach with no backend metadata behind the new ID, so refuse instead.
	if s.provider == nil {
		slog.WarnContext(ctx, "CopySnapshot is not supported without an EBS provider", "sourceSnapshotId", sourceSnapshotID)
		return nil, errors.New(awserrors.ErrorUnsupportedOperation)
	}

	newSnapshotID := utils.GenerateResourceID("snap")
	copied, err := s.provider.CopySnapshot(ctx, ebsprovider.CopySnapshotRequest{
		Versioned:             ebsprovider.NewVersioned(),
		SourceSnapshotID:      sourceSnapshotID,
		DestinationSnapshotID: newSnapshotID,
		VolumeID:              sourceCfg.VolumeID,
	})
	if err != nil || copied == nil {
		slog.ErrorContext(ctx, "CopySnapshot: provider copy failed", "sourceSnapshotId", sourceSnapshotID, "newSnapshotId", newSnapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	now := time.Now()
	snapshotCfg := &SnapshotConfig{
		SnapshotID: newSnapshotID, VolumeID: sourceCfg.VolumeID,
		VolumeSize: sourceCfg.VolumeSize,
		State:      string(copied.State), Progress: "100%", StartTime: now,
		Encrypted: sourceCfg.Encrypted,
		OwnerID:   accountID, AvailabilityZone: sourceCfg.AvailabilityZone,
		Tags:           utils.ExtractTags(input.TagSpecifications, "snapshot"),
		ProviderHandle: copied.Handle,
	}
	if snapshotCfg.State == "" {
		snapshotCfg.State = "completed"
	}
	snapshotCfg.Description = sourceCfg.Description
	if input.Description != nil {
		snapshotCfg.Description = *input.Description
	}

	// The copy pins the same chunks as its source, so it must join the volume's
	// snapshot list before it is visible: a copy missing from that list would
	// let the volume be deleted out from under it.
	if err := s.addSnapshotRef(ctx, sourceCfg.VolumeID, newSnapshotID); err != nil {
		_ = s.provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: newSnapshotID, Handle: copied.Handle})
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if err := s.putSnapshotConfig(newSnapshotID, snapshotCfg); err != nil {
		_ = s.removeSnapshotRefForCleanup(ctx, sourceCfg.VolumeID, newSnapshotID)
		_ = s.provider.DeleteSnapshot(ctx, ebsprovider.DeleteSnapshotRequest{Versioned: ebsprovider.NewVersioned(), SnapshotID: newSnapshotID, Handle: copied.Handle})
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "CopySnapshot completed", "sourceSnapshotId", sourceSnapshotID, "newSnapshotId", newSnapshotID)

	return &ec2.CopySnapshotOutput{
		SnapshotId: aws.String(newSnapshotID),
		Tags:       utils.MapToEC2Tags(snapshotCfg.Tags),
	}, nil
}

// addSnapshotRef adds snapshotID to the volume's snapshot list in KV.
// Uses CAS (Create/Update with revision) to prevent lost updates under concurrency.
func (s *SnapshotServiceImpl) addSnapshotRef(ctx context.Context, volumeID, snapshotID string) error {
	if s.snapKV == nil {
		slog.Debug("addSnapshotRef: snapshotKV is nil, skipping", "volumeId", volumeID, "snapshotId", snapshotID)
		return nil
	}

	const maxRetries = 5
	for attempt := range maxRetries {
		entry, err := s.snapKV.Get(ctx, volumeID)
		var snapshots []string

		if err != nil {
			if !errors.Is(err, jetstream.ErrKeyNotFound) {
				return fmt.Errorf("addSnapshotRef: failed to get KV key %s: %w", volumeID, err)
			}
			// Key doesn't exist yet — create with just this snapshot
			data, err := json.Marshal([]string{snapshotID})
			if err != nil {
				return fmt.Errorf("addSnapshotRef: failed to marshal snapshot list: %w", err)
			}
			if _, err := s.snapKV.Create(ctx, volumeID, data); err != nil {
				if attempt < maxRetries-1 {
					continue // concurrent Create/Update — retry
				}
				return fmt.Errorf("addSnapshotRef: failed to create KV key %s: %w", volumeID, err)
			}
			slog.Info("addSnapshotRef: added snapshot ref", "volumeId", volumeID, "snapshotId", snapshotID)
			return nil
		}

		if err := json.Unmarshal(entry.Value(), &snapshots); err != nil {
			return fmt.Errorf("addSnapshotRef: failed to unmarshal KV value for %s: %w", volumeID, err)
		}

		snapshots = append(snapshots, snapshotID)

		data, err := json.Marshal(snapshots)
		if err != nil {
			return fmt.Errorf("addSnapshotRef: failed to marshal snapshot list: %w", err)
		}

		if _, err := s.snapKV.Update(ctx, volumeID, data, entry.Revision()); err != nil {
			if attempt < maxRetries-1 {
				continue // concurrent update — retry
			}
			return fmt.Errorf("addSnapshotRef: failed to update KV key %s: %w", volumeID, err)
		}

		slog.Info("addSnapshotRef: added snapshot ref", "volumeId", volumeID, "snapshotId", snapshotID)
		return nil
	}

	return fmt.Errorf("addSnapshotRef: exhausted retries for KV key %s", volumeID)
}

// removeSnapshotRefForCleanup removes a reference with a bounded context that
// survives request cancellation after the snapshot operation has committed.
func (s *SnapshotServiceImpl) removeSnapshotRefForCleanup(ctx context.Context, volumeID, snapshotID string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
	defer cancel()
	return s.removeSnapshotRef(cleanupCtx, volumeID, snapshotID)
}

// removeSnapshotRef removes snapshotID from the volume's snapshot list in KV.
// Deletes the key if the list becomes empty.
// Uses CAS (Update with revision) to prevent lost updates under concurrency.
func (s *SnapshotServiceImpl) removeSnapshotRef(ctx context.Context, volumeID, snapshotID string) error {
	if s.snapKV == nil {
		slog.Debug("removeSnapshotRef: snapshotKV is nil, skipping", "volumeId", volumeID, "snapshotId", snapshotID)
		return nil
	}

	const maxRetries = 5
	for attempt := range maxRetries {
		entry, err := s.snapKV.Get(ctx, volumeID)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return nil
			}
			return fmt.Errorf("removeSnapshotRef: failed to get KV key %s: %w", volumeID, err)
		}

		var snapshots []string
		if err := json.Unmarshal(entry.Value(), &snapshots); err != nil {
			return fmt.Errorf("removeSnapshotRef: failed to unmarshal KV value for %s: %w", volumeID, err)
		}

		filtered := snapshots[:0]
		for _, snap := range snapshots {
			if snap != snapshotID {
				filtered = append(filtered, snap)
			}
		}

		if len(filtered) == 0 {
			if err := s.snapKV.Delete(ctx, volumeID); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
				return fmt.Errorf("removeSnapshotRef: failed to delete KV key %s: %w", volumeID, err)
			}
		} else {
			data, err := json.Marshal(filtered)
			if err != nil {
				return fmt.Errorf("removeSnapshotRef: failed to marshal snapshot list: %w", err)
			}
			if _, err := s.snapKV.Update(ctx, volumeID, data, entry.Revision()); err != nil {
				if attempt < maxRetries-1 {
					continue // concurrent update — retry
				}
				return fmt.Errorf("removeSnapshotRef: failed to update KV key %s: %w", volumeID, err)
			}
		}

		slog.Info("removeSnapshotRef: removed snapshot ref", "volumeId", volumeID, "snapshotId", snapshotID)
		return nil
	}

	return fmt.Errorf("removeSnapshotRef: exhausted retries for KV key %s", volumeID)
}

// volumeHasSnapshots returns true if the volume has any snapshots in KV.
func (s *SnapshotServiceImpl) volumeHasSnapshots(ctx context.Context, volumeID string) (bool, error) {
	if s.snapKV == nil {
		return false, nil
	}

	entry, err := s.snapKV.Get(ctx, volumeID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("volumeHasSnapshots: failed to get KV key %s: %w", volumeID, err)
	}

	var snapshots []string
	if err := json.Unmarshal(entry.Value(), &snapshots); err != nil {
		return false, fmt.Errorf("volumeHasSnapshots: failed to unmarshal KV value for %s: %w", volumeID, err)
	}

	return len(snapshots) > 0, nil
}

// ApplyRecordTags mirrors CreateTags into the owning snapshot metadata so
// DescribeSnapshots observes tags added after create. Non-snap ids, snapshots
// absent from this store, and snapshots the caller does not own are skipped.
func (s *SnapshotServiceImpl) ApplyRecordTags(input *ec2.CreateTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return s.mirrorSnapshotTags(input.Resources, accountID, utils.MergeTagsMut(input))
}

// RemoveRecordTags mirrors DeleteTags into the owning snapshot metadata with
// AWS-faithful delete semantics.
func (s *SnapshotServiceImpl) RemoveRecordTags(input *ec2.DeleteTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return s.mirrorSnapshotTags(input.Resources, accountID, utils.RemoveTagsMut(input))
}

// mirrorSnapshotTags read-modify-writes SnapshotConfig.Tags for each snap- id.
// metadata.json lives at a global ID-keyed path, so the mutation is gated on
// the caller owning the snapshot (OwnerID match); mismatch or absence no-ops.
func (s *SnapshotServiceImpl) mirrorSnapshotTags(resources []*string, accountID string, mut func(map[string]string)) error {
	for _, res := range resources {
		if res == nil || !strings.HasPrefix(*res, "snap-") {
			continue
		}
		cfg, err := ReadSnapshotConfig(context.Background(), s.store, s.config.Predastore.Bucket, *res)
		if err != nil {
			if objectstore.IsNoSuchKeyError(err) {
				continue
			}
			return err
		}
		if cfg.OwnerID != accountID {
			slog.Debug("mirrorSnapshotTags: skipping snapshot not owned by caller", "snapshotId", *res)
			continue
		}
		if cfg.Tags == nil {
			cfg.Tags = map[string]string{}
		}
		mut(cfg.Tags)
		if err := s.putSnapshotConfig(*res, cfg); err != nil {
			return err
		}
	}
	return nil
}
