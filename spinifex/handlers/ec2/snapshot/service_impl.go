package handlers_ec2_snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats.go"
)

// Ensure SnapshotServiceImpl implements SnapshotService
var _ SnapshotService = (*SnapshotServiceImpl)(nil)

const (
	KVBucketVolumeSnapshots        = "spinifex-volume-snapshots"
	KVBucketVolumeSnapshotsVersion = 1
)

// SnapshotServiceImpl implements SnapshotService with S3-backed storage
type SnapshotServiceImpl struct {
	config   *config.Config
	store    objectstore.ObjectStore
	natsConn *nats.Conn
	snapKV   nats.KeyValue
	mutex    sync.RWMutex
}

// SnapshotConfig represents snapshot metadata stored in S3
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
}

// NewSnapshotServiceImplWithNATS creates a snapshot service with JetStream KV for volume-snapshot tracking
func NewSnapshotServiceImplWithNATS(cfg *config.Config, natsConn *nats.Conn) (*SnapshotServiceImpl, nats.KeyValue, error) {
	store := objectstore.NewS3ObjectStoreFromConfig(
		cfg.Predastore.Host,
		cfg.Predastore.Region,
		cfg.Predastore.AccessKey,
		cfg.Predastore.SecretKey,
	)

	js, err := natsConn.JetStream()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	kv, err := utils.GetOrCreateKVBucket(js, KVBucketVolumeSnapshots, 10)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create KV bucket %s: %w", KVBucketVolumeSnapshots, err)
	}
	if err := migrate.DefaultRegistry.RunKV(KVBucketVolumeSnapshots, kv, KVBucketVolumeSnapshotsVersion); err != nil {
		return nil, nil, fmt.Errorf("migrate %s: %w", KVBucketVolumeSnapshots, err)
	}

	slog.Info("Snapshot service initialized with JetStream KV", "bucket", KVBucketVolumeSnapshots)

	return &SnapshotServiceImpl{
		config:   cfg,
		store:    store,
		natsConn: natsConn,
		snapKV:   kv,
	}, kv, nil
}

// NewSnapshotServiceImplWithStore creates a snapshot service with a custom ObjectStore (for testing).
// An optional snapshotKV can be provided for KV-backed volume-snapshot tracking.
func NewSnapshotServiceImplWithStore(cfg *config.Config, store objectstore.ObjectStore, natsConn *nats.Conn, snapshotKV ...nats.KeyValue) *SnapshotServiceImpl {
	svc := &SnapshotServiceImpl{
		config:   cfg,
		store:    store,
		natsConn: natsConn,
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
// Decode failures wrap ErrCorruptSnapshotMetadata.
func ReadSnapshotConfig(store objectstore.ObjectStore, bucket, snapshotID string) (*SnapshotConfig, error) {
	key := GetSnapshotKey(snapshotID)
	result, err := store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()

	var cfg SnapshotConfig
	if err := json.NewDecoder(result.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptSnapshotMetadata, err)
	}
	return &cfg, nil
}

// WriteSnapshotConfig writes the SnapshotConfig to {snapshotID}/metadata.json.
func WriteSnapshotConfig(store objectstore.ObjectStore, bucket, snapshotID string, cfg *SnapshotConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = store.PutObject(&s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(GetSnapshotKey(snapshotID)),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}

// getSnapshotConfig translates NoSuchKey to InvalidSnapshot.NotFound.
func (s *SnapshotServiceImpl) getSnapshotConfig(snapshotID string) (*SnapshotConfig, error) {
	cfg, err := ReadSnapshotConfig(s.store, s.config.Predastore.Bucket, snapshotID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
		}
		return nil, err
	}
	return cfg, nil
}

// putSnapshotConfig stores snapshot config to S3
func (s *SnapshotServiceImpl) putSnapshotConfig(snapshotID string, cfg *SnapshotConfig) error {
	return WriteSnapshotConfig(s.store, s.config.Predastore.Bucket, snapshotID, cfg)
}

// snapshotConfigToEC2 converts a SnapshotConfig to an EC2 Snapshot response object
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

// CreateSnapshot creates a new snapshot from a volume
func (s *SnapshotServiceImpl) CreateSnapshot(input *ec2.CreateSnapshotInput, accountID string) (*ec2.Snapshot, error) {
	if input == nil || input.VolumeId == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	volumeID := *input.VolumeId

	slog.Info("CreateSnapshot request", "volumeId", volumeID)

	snapshotID := utils.GenerateResourceID("snap")

	volumeConfigKey := fmt.Sprintf("%s/config.json", volumeID)
	volumeResult, err := s.store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(s.config.Predastore.Bucket),
		Key:    aws.String(volumeConfigKey),
	})
	if err != nil {
		slog.Error("CreateSnapshot failed to get volume config", "volumeId", volumeID, "err", err)
		return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}
	defer volumeResult.Body.Close()

	volumeBody, err := io.ReadAll(volumeResult.Body)
	if err != nil {
		slog.Error("CreateSnapshot failed to read volume config", "volumeId", volumeID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// config.json may be an at-rest encryption envelope; StateBody unwraps it to
	// the inner VBState. Decoding the raw envelope yields a zero state
	// (SizeGiB==0), which the size guard below would reject as a 500.
	var volumeState viperblock.VBState
	if err := json.Unmarshal(viperblock.StateBody(volumeBody), &volumeState); err != nil {
		slog.Error("CreateSnapshot failed to decode volume config", "volumeId", volumeID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	volumeConfig := volumeState.VolumeConfig

	// Verify the caller owns the source volume
	if accountID != "" && volumeConfig.VolumeMetadata.TenantID != "" && volumeConfig.VolumeMetadata.TenantID != accountID {
		slog.Warn("CreateSnapshot: account does not own volume", "volumeId", volumeID, "accountID", accountID, "tenantID", volumeConfig.VolumeMetadata.TenantID)
		return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}

	if volumeConfig.VolumeMetadata.SizeGiB == 0 {
		slog.Error("CreateSnapshot: source volume has zero size in config", "volumeId", volumeID)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Trigger viperblock to flush data and create a frozen block map checkpoint.
	// This sends a NATS message to the EBS daemon that owns the volume, which
	// calls vb.CreateSnapshot() on the live viperblock instance.
	if s.natsConn != nil {
		snapReq := types.EBSSnapshotRequest{Volume: volumeID, SnapshotID: snapshotID}
		snapData, err := json.Marshal(snapReq)
		if err != nil {
			slog.Error("CreateSnapshot: failed to marshal ebs.snapshot request", "volumeId", volumeID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}

		msg, err := s.natsConn.Request(fmt.Sprintf("ebs.snapshot.%s", volumeID), snapData, 30*time.Second)
		if errors.Is(err, nats.ErrNoResponders) {
			// Volume is not mounted — data is already persisted to S3, proceed with metadata-only snapshot.
			slog.Info("CreateSnapshot: volume not mounted, creating metadata-only snapshot", "volumeId", volumeID, "snapshotId", snapshotID)
		} else if err != nil {
			slog.Error("CreateSnapshot: ebs.snapshot NATS request failed", "volumeId", volumeID, "snapshotId", snapshotID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		} else {
			var snapResp types.EBSSnapshotResponse
			if err := json.Unmarshal(msg.Data, &snapResp); err != nil {
				slog.Error("CreateSnapshot: failed to unmarshal ebs.snapshot response", "volumeId", volumeID, "snapshotId", snapshotID, "err", err)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			if !snapResp.Success || snapResp.Error != "" {
				slog.Error("CreateSnapshot: viperblock snapshot failed", "volumeId", volumeID, "snapshotId", snapshotID, "err", snapResp.Error)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}

			slog.Info("CreateSnapshot: viperblock snapshot created", "volumeId", volumeID, "snapshotId", snapshotID)
		}
	} else {
		slog.Warn("CreateSnapshot: natsConn is nil, skipping viperblock snapshot (metadata-only)", "volumeId", volumeID)
	}

	now := time.Now()

	snapshotCfg := &SnapshotConfig{
		SnapshotID:       snapshotID,
		VolumeID:         volumeID,
		VolumeSize:       utils.SafeUint64ToInt64(volumeConfig.VolumeMetadata.SizeGiB),
		State:            "completed",
		Progress:         "100%",
		StartTime:        now,
		Encrypted:        volumeState.EncryptionEnabled,
		OwnerID:          accountID,
		AvailabilityZone: volumeConfig.VolumeMetadata.AvailabilityZone,
		Tags:             utils.ExtractTags(input.TagSpecifications, "snapshot"),
	}

	if input.Description != nil {
		snapshotCfg.Description = *input.Description
	}

	// Track the volume→snapshot dependency in KV before persisting to S3.
	// This ensures we never have an untracked snapshot in S3.
	if err := s.addSnapshotRef(volumeID, snapshotID); err != nil {
		slog.Error("CreateSnapshot failed to add snapshot ref to KV", "snapshotId", snapshotID, "volumeId", volumeID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if err := s.putSnapshotConfig(snapshotID, snapshotCfg); err != nil {
		slog.Error("CreateSnapshot failed to write config", "snapshotId", snapshotID, "err", err)
		_ = s.removeSnapshotRef(volumeID, snapshotID) // best-effort cleanup
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CreateSnapshot completed", "snapshotId", snapshotID, "volumeId", volumeID)

	return snapshotConfigToEC2(snapshotCfg), nil
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
func (s *SnapshotServiceImpl) DescribeSnapshots(input *ec2.DescribeSnapshotsInput, accountID string) (*ec2.DescribeSnapshotsOutput, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	slog.Info("DescribeSnapshots request", "snapshotIds", input.SnapshotIds, "accountID", accountID)

	snapshotIDFilter := make(map[string]bool)
	for _, id := range input.SnapshotIds {
		if id != nil {
			snapshotIDFilter[*id] = true
		}
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeSnapshotsValidFilters)
	if err != nil {
		slog.Warn("DescribeSnapshots: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	listResult, err := s.store.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket:    aws.String(s.config.Predastore.Bucket),
		Prefix:    aws.String("snap-"),
		Delimiter: aws.String("/"),
	})
	if err != nil {
		slog.Error("DescribeSnapshots failed to list objects", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Extract snapshot-id filter values for early prefix skipping to avoid
	// unnecessary S3 GetObject calls on non-matching snapshots.
	var snapshotIDFilterValues []string
	if parsedFilters != nil {
		snapshotIDFilterValues = parsedFilters["snapshot-id"]
	}

	var snapshots []*ec2.Snapshot
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

		cfg, err := s.getSnapshotConfig(snapshotID)
		if err != nil {
			slog.Warn("DescribeSnapshots failed to get config", "snapshotId", snapshotID, "err", err)
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

	slog.Info("DescribeSnapshots completed", "count", len(snapshots))

	return &ec2.DescribeSnapshotsOutput{
		Snapshots: snapshots,
	}, nil
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

// snapshotInUseByVolumes checks if any volume was created from the given snapshot.
func (s *SnapshotServiceImpl) snapshotInUseByVolumes(snapshotID string) (bool, error) {
	listResult, err := s.store.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket:    aws.String(s.config.Predastore.Bucket),
		Prefix:    aws.String("vol-"),
		Delimiter: aws.String("/"),
	})
	if err != nil {
		return false, fmt.Errorf("snapshotInUseByVolumes: failed to list volumes: %w", err)
	}

	for _, prefix := range listResult.CommonPrefixes {
		if prefix.Prefix == nil {
			continue
		}
		volumeID := strings.TrimSuffix(*prefix.Prefix, "/")
		configKey := fmt.Sprintf("%s/config.json", volumeID)

		result, err := s.store.GetObject(&s3.GetObjectInput{
			Bucket: aws.String(s.config.Predastore.Bucket),
			Key:    aws.String(configKey),
		})
		if err != nil {
			continue // volume may not have a config yet
		}

		scanBody, readErr := io.ReadAll(result.Body)
		_ = result.Body.Close()
		if readErr != nil {
			continue
		}
		// Unwrap the encryption envelope so encrypted volumes are scanned too;
		// a raw decode yields a zero state and silently drops their snapshots.
		var state viperblock.VBState
		if decodeErr := json.Unmarshal(viperblock.StateBody(scanBody), &state); decodeErr != nil {
			continue
		}

		if state.VolumeConfig.VolumeMetadata.SnapshotID == snapshotID {
			return true, nil
		}
	}

	return false, nil
}

// DeleteSnapshot deletes a snapshot after verifying the caller owns it.
func (s *SnapshotServiceImpl) DeleteSnapshot(input *ec2.DeleteSnapshotInput, accountID string) (*ec2.DeleteSnapshotOutput, error) {
	if input == nil || input.SnapshotId == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	snapshotID := *input.SnapshotId

	slog.Info("DeleteSnapshot request", "snapshotId", snapshotID, "accountID", accountID)

	cfg, err := s.getSnapshotConfig(snapshotID)
	if err != nil {
		slog.Error("DeleteSnapshot snapshot not found", "snapshotId", snapshotID, "err", err)
		return nil, err
	}

	// Verify ownership: caller must own the snapshot
	if accountID != "" && cfg.OwnerID != "" && cfg.OwnerID != accountID {
		slog.Warn("DeleteSnapshot: account does not own snapshot", "snapshotId", snapshotID, "accountID", accountID, "ownerID", cfg.OwnerID)
		return nil, errors.New(awserrors.ErrorUnauthorizedOperation)
	}

	// Check if any volumes were created from this snapshot
	inUse, err := s.snapshotInUseByVolumes(snapshotID)
	if err != nil {
		slog.Error("DeleteSnapshot failed to check snapshot usage", "snapshotId", snapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if inUse {
		slog.Info("DeleteSnapshot blocked: snapshot in use by volume", "snapshotId", snapshotID)
		return nil, errors.New(awserrors.ErrorInvalidSnapshotInUse)
	}

	listResult, err := s.store.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket: aws.String(s.config.Predastore.Bucket),
		Prefix: aws.String(snapshotID + "/"),
	})
	if err != nil {
		slog.Error("DeleteSnapshot failed to list objects", "snapshotId", snapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	for _, obj := range listResult.Contents {
		if obj.Key == nil {
			continue
		}
		_, err := s.store.DeleteObject(&s3.DeleteObjectInput{
			Bucket: aws.String(s.config.Predastore.Bucket),
			Key:    obj.Key,
		})
		if err != nil {
			slog.Warn("DeleteSnapshot failed to delete object", "key", *obj.Key, "err", err)
		}
	}

	// Remove from KV after S3 cleanup. Failure is logged but not fatal —
	// a phantom entry safely blocks volume deletion rather than allowing it.
	if err := s.removeSnapshotRef(cfg.VolumeID, snapshotID); err != nil {
		slog.Warn("DeleteSnapshot failed to remove snapshot ref from KV", "snapshotId", snapshotID, "volumeId", cfg.VolumeID, "err", err)
	}

	slog.Info("DeleteSnapshot completed", "snapshotId", snapshotID)

	return &ec2.DeleteSnapshotOutput{}, nil
}

// CopySnapshot copies a snapshot (within same region for now).
// The copied snapshot is owned by the caller's account.
func (s *SnapshotServiceImpl) CopySnapshot(input *ec2.CopySnapshotInput, accountID string) (*ec2.CopySnapshotOutput, error) {
	if input == nil || input.SourceSnapshotId == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	sourceSnapshotID := *input.SourceSnapshotId

	slog.Info("CopySnapshot request", "sourceSnapshotId", sourceSnapshotID, "accountID", accountID)

	sourceCfg, err := s.getSnapshotConfig(sourceSnapshotID)
	if err != nil {
		slog.Error("CopySnapshot source snapshot not found", "snapshotId", sourceSnapshotID, "err", err)
		return nil, err
	}

	// Verify the caller owns the source snapshot
	if accountID != "" && sourceCfg.OwnerID != "" && sourceCfg.OwnerID != accountID {
		slog.Warn("CopySnapshot: account does not own source snapshot", "snapshotId", sourceSnapshotID, "accountID", accountID, "ownerID", sourceCfg.OwnerID)
		return nil, errors.New(awserrors.ErrorUnauthorizedOperation)
	}

	newSnapshotID := utils.GenerateResourceID("snap")

	newCfg := &SnapshotConfig{
		SnapshotID:       newSnapshotID,
		VolumeID:         sourceCfg.VolumeID,
		VolumeSize:       sourceCfg.VolumeSize,
		State:            "completed",
		Progress:         "100%",
		StartTime:        time.Now(),
		Description:      sourceCfg.Description,
		Encrypted:        sourceCfg.Encrypted,
		OwnerID:          accountID,
		AvailabilityZone: sourceCfg.AvailabilityZone,
		Tags:             make(map[string]string),
	}

	if input.Description != nil {
		newCfg.Description = *input.Description
	}

	maps.Copy(newCfg.Tags, sourceCfg.Tags)

	// Track the volume→snapshot dependency in KV before persisting to S3.
	if err := s.addSnapshotRef(sourceCfg.VolumeID, newSnapshotID); err != nil {
		slog.Error("CopySnapshot failed to add snapshot ref to KV", "snapshotId", newSnapshotID, "volumeId", sourceCfg.VolumeID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if err := s.putSnapshotConfig(newSnapshotID, newCfg); err != nil {
		slog.Error("CopySnapshot failed to write config", "snapshotId", newSnapshotID, "err", err)
		_ = s.removeSnapshotRef(sourceCfg.VolumeID, newSnapshotID) // best-effort cleanup
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CopySnapshot completed", "sourceSnapshotId", sourceSnapshotID, "newSnapshotId", newSnapshotID)

	return &ec2.CopySnapshotOutput{
		SnapshotId: aws.String(newSnapshotID),
	}, nil
}

// addSnapshotRef adds snapshotID to the volume's snapshot list in KV.
// Uses CAS (Create/Update with revision) to prevent lost updates under concurrency.
func (s *SnapshotServiceImpl) addSnapshotRef(volumeID, snapshotID string) error {
	if s.snapKV == nil {
		slog.Debug("addSnapshotRef: snapshotKV is nil, skipping", "volumeId", volumeID, "snapshotId", snapshotID)
		return nil
	}

	const maxRetries = 5
	for attempt := range maxRetries {
		entry, err := s.snapKV.Get(volumeID)
		var snapshots []string

		if err != nil {
			if !errors.Is(err, nats.ErrKeyNotFound) {
				return fmt.Errorf("addSnapshotRef: failed to get KV key %s: %w", volumeID, err)
			}
			// Key doesn't exist yet — create with just this snapshot
			data, err := json.Marshal([]string{snapshotID})
			if err != nil {
				return fmt.Errorf("addSnapshotRef: failed to marshal snapshot list: %w", err)
			}
			if _, err := s.snapKV.Create(volumeID, data); err != nil {
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

		if _, err := s.snapKV.Update(volumeID, data, entry.Revision()); err != nil {
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

// removeSnapshotRef removes snapshotID from the volume's snapshot list in KV.
// Deletes the key if the list becomes empty.
// Uses CAS (Update with revision) to prevent lost updates under concurrency.
func (s *SnapshotServiceImpl) removeSnapshotRef(volumeID, snapshotID string) error {
	if s.snapKV == nil {
		slog.Debug("removeSnapshotRef: snapshotKV is nil, skipping", "volumeId", volumeID, "snapshotId", snapshotID)
		return nil
	}

	const maxRetries = 5
	for attempt := range maxRetries {
		entry, err := s.snapKV.Get(volumeID)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
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
			if err := s.snapKV.Delete(volumeID); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
				return fmt.Errorf("removeSnapshotRef: failed to delete KV key %s: %w", volumeID, err)
			}
		} else {
			data, err := json.Marshal(filtered)
			if err != nil {
				return fmt.Errorf("removeSnapshotRef: failed to marshal snapshot list: %w", err)
			}
			if _, err := s.snapKV.Update(volumeID, data, entry.Revision()); err != nil {
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
func (s *SnapshotServiceImpl) volumeHasSnapshots(volumeID string) (bool, error) {
	if s.snapKV == nil {
		return false, nil
	}

	entry, err := s.snapKV.Get(volumeID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
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
