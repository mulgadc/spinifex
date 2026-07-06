package handlers_ec2_volume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/viperblock"
	s3backend "github.com/mulgadc/viperblock/viperblock/backends/s3"
	"github.com/nats-io/nats.go"
)

const (
	// gp3 IOPS envelope (AWS): 3000 baseline on any size, up to 500 IOPS/GiB,
	// capped at 16000.
	defaultGP3IOPS = 3000
	maxGP3IOPS     = 16000
	gp3IOPSPerGiB  = 500
)

// Ensure VolumeServiceImpl implements VolumeService
var _ VolumeService = (*VolumeServiceImpl)(nil)

// Ensure VolumeServiceImpl satisfies vm.VolumeStateUpdater so the manager
// can call UpdateVolumeState directly without a daemon-side adapter.
var _ vm.VolumeStateUpdater = (*VolumeServiceImpl)(nil)

// VolumeServiceImpl handles EBS volume operations with S3 storage
type VolumeServiceImpl struct {
	config     *config.Config
	store      objectstore.ObjectStore
	bucketName string
	natsConn   *nats.Conn
	snapshotKV nats.KeyValue
}

// NewVolumeServiceImpl creates a new daemon-side volume service.
// snapshotKV is optional — when non-nil, DeleteVolume uses O(1) KV lookup
// instead of scanning all snapshots in S3.
func NewVolumeServiceImpl(cfg *config.Config, natsConn *nats.Conn, snapshotKV nats.KeyValue) *VolumeServiceImpl {
	store := objectstore.NewS3ObjectStoreFromConfig(
		cfg.Predastore.Host,
		cfg.Predastore.Region,
		cfg.Predastore.AccessKey,
		cfg.Predastore.SecretKey,
	)

	return &VolumeServiceImpl{
		config:     cfg,
		store:      store,
		bucketName: cfg.Predastore.Bucket,
		natsConn:   natsConn,
		snapshotKV: snapshotKV,
	}
}

// NewVolumeServiceImplWithStore creates a volume service with a custom ObjectStore (for testing)
func NewVolumeServiceImplWithStore(cfg *config.Config, store objectstore.ObjectStore, natsConn *nats.Conn, snapshotKV ...nats.KeyValue) *VolumeServiceImpl {
	bucketName := ""
	if cfg != nil {
		bucketName = cfg.Predastore.Bucket
	}
	svc := &VolumeServiceImpl{
		config:     cfg,
		store:      store,
		bucketName: bucketName,
		natsConn:   natsConn,
	}
	if len(snapshotKV) > 0 {
		svc.snapshotKV = snapshotKV[0]
	}
	return svc
}

// CreateVolume creates a new EBS volume via viperblock and persists its config to S3
func (s *VolumeServiceImpl) CreateVolume(ctx context.Context, input *ec2.CreateVolumeInput, accountID string) (*ec2.Volume, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Validate volume type: only gp3 supported (or empty defaults to gp3)
	if input.VolumeType != nil && *input.VolumeType != "" && *input.VolumeType != "gp3" {
		return nil, errors.New(awserrors.ErrorUnknownVolumeType)
	}
	volumeType := "gp3"

	// Validate availability zone matches this node's AZ
	if input.AvailabilityZone == nil || *input.AvailabilityZone == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if *input.AvailabilityZone != s.config.AZ {
		return nil, errors.New(awserrors.ErrorInvalidAvailabilityZone)
	}

	// If creating from snapshot, read snapshot metadata to get defaults
	var snapshotID string
	var sourceVolumeName string
	var snapshotSizeGiB int64

	if input.SnapshotId != nil && *input.SnapshotId != "" {
		snapshotID = *input.SnapshotId
		snapMeta, err := s.getSnapshotMetadata(ctx, snapshotID)
		if err != nil {
			slog.ErrorContext(ctx, "CreateVolume: snapshot not found", "snapshotId", snapshotID, "err", err)
			return nil, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
		}
		sourceVolumeName = snapMeta.VolumeID
		snapshotSizeGiB = snapMeta.VolumeSize
	}

	// Validate size (1-16384 GiB). When creating from snapshot, size can be
	// omitted (defaults to snapshot size) or must be >= snapshot size.
	var size int64
	if input.Size != nil {
		if *input.Size < 1 || *input.Size > 16384 {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		if snapshotSizeGiB > 0 && *input.Size < snapshotSizeGiB {
			slog.ErrorContext(ctx, "CreateVolume: requested size smaller than snapshot", "size", *input.Size, "snapshotSize", snapshotSizeGiB)
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		size = *input.Size
	} else if snapshotSizeGiB > 0 {
		size = snapshotSizeGiB
	} else {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	now := time.Now()
	volumeID := utils.GenerateResourceID("vol")

	// Honor caller-supplied Iops for gp3, else the 3000 baseline. The ceiling is
	// min(16000, 500*size) but never below the free baseline, so small volumes
	// still get 3000.
	iops := defaultGP3IOPS
	if input.Iops != nil {
		iops = int(*input.Iops)
	}
	maxIOPS := min(max(int(size)*gp3IOPSPerGiB, defaultGP3IOPS), maxGP3IOPS)
	if iops < defaultGP3IOPS || iops > maxIOPS {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	slog.InfoContext(ctx, "CreateVolume", "volumeId", volumeID, "size", size, "type", volumeType,
		"az", *input.AvailabilityZone, "snapshotId", snapshotID)

	tags := utils.ExtractTags(input.TagSpecifications, "volume")

	// Volume size in bytes for viperblock
	sizeGiB := utils.SafeInt64ToUint64(size)
	volumeSizeBytes := sizeGiB * 1024 * 1024 * 1024

	// Build VolumeConfig with metadata
	volumeConfig := viperblock.VolumeConfig{
		VolumeMetadata: viperblock.VolumeMetadata{
			VolumeID:         volumeID,
			TenantID:         accountID,
			SizeGiB:          sizeGiB,
			State:            "available",
			CreatedAt:        now,
			AvailabilityZone: *input.AvailabilityZone,
			VolumeType:       volumeType,
			IOPS:             iops,
			SnapshotID:       snapshotID,
			Tags:             tags,
		},
	}

	// Create S3 backend config
	cfg := s3backend.S3Config{
		VolumeName: volumeID,
		VolumeSize: volumeSizeBytes,
		Bucket:     s.bucketName,
		Region:     s.config.Predastore.Region,
		AccessKey:  s.config.Predastore.AccessKey,
		SecretKey:  s.config.Predastore.SecretKey,
		Host:       s.config.Predastore.Host,
	}

	mkey, err := utils.LoadViperblockMasterKey(s.config.Viperblock.EncryptionKeyFile)
	if err != nil {
		slog.ErrorContext(ctx, "CreateVolume failed to load encryption key", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	vbconfig := viperblock.VB{
		VolumeName:        volumeID,
		VolumeSize:        volumeSizeBytes,
		BaseDir:           s.config.WalDir,
		Cache:             viperblock.Cache{Config: viperblock.CacheConfig{Size: 0}},
		VolumeConfig:      volumeConfig,
		MasterKey:         mkey,
		EncryptionEnabled: mkey != nil,
	}

	// If created from a snapshot, set the snapshot fields so viperblock's
	// LoadState will call OpenFromSnapshot to load the base block map.
	if snapshotID != "" {
		vbconfig.SnapshotID = snapshotID
		vbconfig.SourceVolumeName = sourceVolumeName
	}

	vb, err := viperblock.New(&vbconfig, "s3", cfg)
	if err != nil {
		slog.ErrorContext(ctx, "CreateVolume failed to create viperblock instance", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	vb.SetDebug(false)

	// Initialize the backend (creates bucket structure in S3)
	if err := vb.Backend.Init(); err != nil {
		slog.ErrorContext(ctx, "CreateVolume failed to initialize backend", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Persist volume state to S3 (writes config.json)
	if err := vb.SaveState(); err != nil {
		slog.ErrorContext(ctx, "CreateVolume failed to save state", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "CreateVolume completed", "volumeId", volumeID, "size", size, "type", volumeType)

	vol := &ec2.Volume{
		VolumeId:         aws.String(volumeID),
		Size:             aws.Int64(size),
		VolumeType:       aws.String(volumeType),
		State:            aws.String("available"),
		AvailabilityZone: input.AvailabilityZone,
		CreateTime:       aws.Time(now),
		Iops:             aws.Int64(int64(iops)),
		Encrypted:        aws.Bool(mkey != nil),
		Tags:             utils.MapToEC2Tags(tags),
	}

	if snapshotID != "" {
		vol.SnapshotId = aws.String(snapshotID)
	}

	return vol, nil
}

// describeVolumesValidFilters defines the set of filter names accepted by DescribeVolumes.
var describeVolumesValidFilters = map[string]bool{
	"volume-id":              true,
	"status":                 true,
	"size":                   true,
	"volume-type":            true,
	"attachment.instance-id": true,
	"attachment.status":      true,
	"attachment.device":      true,
	"availability-zone":      true,
}

// DescribeVolumes lists EBS volumes by reading config.json files from S3
func (s *VolumeServiceImpl) DescribeVolumes(ctx context.Context, input *ec2.DescribeVolumesInput, accountID string) (*ec2.DescribeVolumesOutput, error) {
	if input == nil {
		input = &ec2.DescribeVolumesInput{}
	}

	slog.InfoContext(ctx, "Describing volumes", "volumeIds", input.VolumeIds)

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeVolumesValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeVolumes: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	var volumes []*ec2.Volume

	// Fast path: if specific volume IDs are requested, fetch them directly
	if len(input.VolumeIds) > 0 {
		// Count non-nil requested IDs
		requested := 0
		for _, id := range input.VolumeIds {
			if id != nil {
				requested++
			}
		}
		volumes = s.fetchVolumesByIDs(ctx, input.VolumeIds, accountID)
		// AWS returns InvalidVolume.NotFound if any requested ID is missing
		if len(volumes) != requested {
			return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
		}

		// Apply filters to the fetched volumes
		if len(parsedFilters) > 0 {
			filtered := make([]*ec2.Volume, 0, len(volumes))
			for _, vol := range volumes {
				if volumeMatchesFilters(vol, parsedFilters) {
					filtered = append(filtered, vol)
				}
			}
			volumes = filtered
		}

		slog.InfoContext(ctx, "DescribeVolumes completed", "count", len(volumes))
		return &ec2.DescribeVolumesOutput{Volumes: volumes}, nil
	}

	// Slow path: list all volumes (no specific IDs requested)
	volumeIDs, err := s.listAllVolumeIDs(ctx)
	if err != nil {
		return nil, err
	}

	// Extract volume-id filter values for early skipping to avoid
	// unnecessary S3 GetObject calls on non-matching volumes.
	var volumeIDFilterValues []string
	if parsedFilters != nil {
		volumeIDFilterValues = parsedFilters["volume-id"]
	}

	for _, volumeID := range volumeIDs {
		// Early skip: if volume-id filter is set, check the ID before
		// fetching the full volume config from S3.
		if len(volumeIDFilterValues) > 0 {
			if !filterutil.MatchesAny(volumeIDFilterValues, volumeID) {
				continue
			}
		}

		result, err := s.getVolumeByID(ctx, volumeID)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to get volume", "volumeId", volumeID, "err", err)
			continue
		}

		// Skip volumes not owned by the caller's account.
		if result.tenantID != accountID {
			continue
		}

		if len(parsedFilters) > 0 && !volumeMatchesFilters(result.volume, parsedFilters) {
			continue
		}

		volumes = append(volumes, result.volume)
	}

	slog.InfoContext(ctx, "DescribeVolumes completed", "count", len(volumes))

	return &ec2.DescribeVolumesOutput{
		Volumes: volumes,
	}, nil
}

// volumeMatchesFilters checks whether an ec2.Volume satisfies all parsed filters.
func volumeMatchesFilters(vol *ec2.Volume, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}

		var field string
		switch name {
		case "volume-id":
			if vol.VolumeId != nil {
				field = *vol.VolumeId
			}
		case "status":
			if vol.State != nil {
				field = *vol.State
			}
		case "size":
			if vol.Size != nil {
				field = strconv.FormatInt(*vol.Size, 10)
			}
		case "volume-type":
			if vol.VolumeType != nil {
				field = *vol.VolumeType
			}
		case "attachment.instance-id":
			if !volumeAttachmentMatchesAny(vol.Attachments, func(a *ec2.VolumeAttachment) string {
				if a.InstanceId != nil {
					return *a.InstanceId
				}
				return ""
			}, values) {
				return false
			}
			continue
		case "attachment.status":
			if !volumeAttachmentMatchesAny(vol.Attachments, func(a *ec2.VolumeAttachment) string {
				if a.State != nil {
					return *a.State
				}
				return ""
			}, values) {
				return false
			}
			continue
		case "attachment.device":
			if !volumeAttachmentMatchesAny(vol.Attachments, func(a *ec2.VolumeAttachment) string {
				if a.Device != nil {
					return *a.Device
				}
				return ""
			}, values) {
				return false
			}
			continue
		case "availability-zone":
			if vol.AvailabilityZone != nil {
				field = *vol.AvailabilityZone
			}
		default:
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	// Check tag:Key filters
	tags := filterutil.EC2TagsToMap(vol.Tags)
	return filterutil.MatchesTags(filters, tags)
}

// volumeAttachmentMatchesAny checks if any attachment's field matches any filter value.
func volumeAttachmentMatchesAny(attachments []*ec2.VolumeAttachment, fieldFn func(*ec2.VolumeAttachment) string, values []string) bool {
	if len(attachments) == 0 {
		return false
	}
	for _, a := range attachments {
		if filterutil.MatchesAny(values, fieldFn(a)) {
			return true
		}
	}
	return false
}

// DescribeVolumeStatus returns the status of one or more EBS volumes
// describeVolumeStatusValidFilters defines the set of filter names accepted by DescribeVolumeStatus.
var describeVolumeStatusValidFilters = map[string]bool{
	"volume-id":            true,
	"volume-status.status": true,
	"availability-zone":    true,
}

func (s *VolumeServiceImpl) DescribeVolumeStatus(ctx context.Context, input *ec2.DescribeVolumeStatusInput, accountID string) (*ec2.DescribeVolumeStatusOutput, error) {
	if input == nil {
		input = &ec2.DescribeVolumeStatusInput{}
	}

	slog.InfoContext(ctx, "DescribeVolumeStatus", "volumeIds", input.VolumeIds)

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeVolumeStatusValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeVolumeStatus: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	var statusItems []*ec2.VolumeStatusItem

	// Fast path: if specific volume IDs are requested, fetch them directly
	if len(input.VolumeIds) > 0 {
		for _, vid := range input.VolumeIds {
			if vid == nil {
				continue
			}
			item, tenantID, err := s.getVolumeStatusByID(ctx, *vid)
			if err != nil {
				slog.ErrorContext(ctx, "DescribeVolumeStatus volume not found", "volumeId", *vid, "err", err)
				return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
			}
			// Skip volumes not owned by the caller's account
			if tenantID != accountID {
				return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
			}
			if len(parsedFilters) > 0 && !volumeStatusMatchesFilters(item, parsedFilters) {
				continue
			}
			statusItems = append(statusItems, item)
		}
		slog.InfoContext(ctx, "DescribeVolumeStatus completed", "count", len(statusItems))
		return &ec2.DescribeVolumeStatusOutput{VolumeStatuses: statusItems}, nil
	}

	// Slow path: list all volumes (no specific IDs requested)
	volumeIDs, err := s.listAllVolumeIDs(ctx)
	if err != nil {
		return nil, err
	}

	// Extract volume-id filter values for early skipping to avoid
	// unnecessary S3 GetObject calls on non-matching volumes.
	var volumeStatusIDFilterValues []string
	if parsedFilters != nil {
		volumeStatusIDFilterValues = parsedFilters["volume-id"]
	}

	for _, volumeID := range volumeIDs {
		// Early skip: if volume-id filter is set, check the ID before
		// fetching the full volume status from S3.
		if len(volumeStatusIDFilterValues) > 0 {
			if !filterutil.MatchesAny(volumeStatusIDFilterValues, volumeID) {
				continue
			}
		}

		item, tenantID, err := s.getVolumeStatusByID(ctx, volumeID)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to get volume status", "volumeId", volumeID, "err", err)
			continue
		}

		// Skip volumes not owned by the caller's account.
		if tenantID != accountID {
			continue
		}

		if len(parsedFilters) > 0 && !volumeStatusMatchesFilters(item, parsedFilters) {
			continue
		}

		statusItems = append(statusItems, item)
	}

	slog.InfoContext(ctx, "DescribeVolumeStatus completed", "count", len(statusItems))

	return &ec2.DescribeVolumeStatusOutput{
		VolumeStatuses: statusItems,
	}, nil
}

// volumeStatusMatchesFilters checks whether a VolumeStatusItem satisfies all parsed filters.
func volumeStatusMatchesFilters(item *ec2.VolumeStatusItem, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			// VolumeStatusItems don't have tags; any tag filter means no match.
			return false
		}

		var field string
		switch name {
		case "volume-id":
			if item.VolumeId != nil {
				field = *item.VolumeId
			}
		case "volume-status.status":
			if item.VolumeStatus != nil && item.VolumeStatus.Status != nil {
				field = *item.VolumeStatus.Status
			}
		case "availability-zone":
			if item.AvailabilityZone != nil {
				field = *item.AvailabilityZone
			}
		default:
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}
	return true
}

// getVolumeStatusByID builds a VolumeStatusItem by reusing getVolumeByID
// to validate the volume exists, then returning static health status.
// Returns the status item, the tenant ID for account scoping, and any error.
func (s *VolumeServiceImpl) getVolumeStatusByID(ctx context.Context, volumeID string) (*ec2.VolumeStatusItem, string, error) {
	result, err := s.getVolumeByID(ctx, volumeID)
	if err != nil {
		return nil, "", err
	}

	return &ec2.VolumeStatusItem{
		VolumeId:         result.volume.VolumeId,
		AvailabilityZone: result.volume.AvailabilityZone,
		VolumeStatus: &ec2.VolumeStatusInfo{
			Status: aws.String("ok"),
			Details: []*ec2.VolumeStatusDetails{
				{
					Name:   aws.String("io-enabled"),
					Status: aws.String("passed"),
				},
				{
					Name:   aws.String("io-performance"),
					Status: aws.String("not-applicable"),
				},
			},
		},
		Actions: []*ec2.VolumeStatusAction{},
		Events:  []*ec2.VolumeStatusEvent{},
	}, result.tenantID, nil
}

// volumeModificationTimeFormat is the AWS-CLI compatible RFC3339-ish format
// used both for response serialisation and for filter equality on time fields.
// Round-tripping a value through this format and back into a filter must match.
const volumeModificationTimeFormat = "2006-01-02T15:04:05.000Z"

// describeVolumesModificationsValidFilters defines the set of filter names
// accepted by DescribeVolumesModifications.
var describeVolumesModificationsValidFilters = map[string]bool{
	"modification-state":   true,
	"original-iops":        true,
	"original-size":        true,
	"original-volume-type": true,
	"start-time":           true,
	"target-iops":          true,
	"target-size":          true,
	"target-volume-type":   true,
	"volume-id":            true,
}

// vbModificationToEC2 converts a persisted viperblock.VolumeModification into
// the AWS SDK shape returned by ModifyVolume / DescribeVolumesModifications.
func vbModificationToEC2(m *viperblock.VolumeModification) *ec2.VolumeModification {
	if m == nil {
		return nil
	}
	out := &ec2.VolumeModification{
		VolumeId:           aws.String(m.VolumeID),
		ModificationState:  aws.String(m.ModificationState),
		Progress:           aws.Int64(m.Progress),
		OriginalSize:       aws.Int64(m.OriginalSize),
		OriginalIops:       aws.Int64(m.OriginalIops),
		OriginalVolumeType: aws.String(m.OriginalVolumeType),
		TargetSize:         aws.Int64(m.TargetSize),
		TargetIops:         aws.Int64(m.TargetIops),
		TargetVolumeType:   aws.String(m.TargetVolumeType),
		StartTime:          aws.Time(m.StartTime),
	}
	if !m.EndTime.IsZero() {
		out.EndTime = aws.Time(m.EndTime)
	}
	if m.StatusMessage != "" {
		out.StatusMessage = aws.String(m.StatusMessage)
	}
	return out
}

// volumeModificationMatchesFilters checks whether an ec2.VolumeModification
// satisfies all parsed filters.
func volumeModificationMatchesFilters(m *ec2.VolumeModification, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			// Modifications don't carry tags; any tag filter means no match.
			return false
		}

		var field string
		switch name {
		case "volume-id":
			if m.VolumeId != nil {
				field = *m.VolumeId
			}
		case "modification-state":
			if m.ModificationState != nil {
				field = *m.ModificationState
			}
		case "original-iops":
			if m.OriginalIops != nil {
				field = strconv.FormatInt(*m.OriginalIops, 10)
			}
		case "original-size":
			if m.OriginalSize != nil {
				field = strconv.FormatInt(*m.OriginalSize, 10)
			}
		case "original-volume-type":
			if m.OriginalVolumeType != nil {
				field = *m.OriginalVolumeType
			}
		case "target-iops":
			if m.TargetIops != nil {
				field = strconv.FormatInt(*m.TargetIops, 10)
			}
		case "target-size":
			if m.TargetSize != nil {
				field = strconv.FormatInt(*m.TargetSize, 10)
			}
		case "target-volume-type":
			if m.TargetVolumeType != nil {
				field = *m.TargetVolumeType
			}
		case "start-time":
			if m.StartTime != nil {
				field = m.StartTime.UTC().Format(volumeModificationTimeFormat)
			}
		default:
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}
	return true
}

// DescribeVolumesModifications returns the most recent modification record
// for one or more EBS volumes. Volumes that have never been modified are
// silently omitted from both fast and slow paths.
func (s *VolumeServiceImpl) DescribeVolumesModifications(ctx context.Context, input *ec2.DescribeVolumesModificationsInput, accountID string) (*ec2.DescribeVolumesModificationsOutput, error) {
	if input == nil {
		input = &ec2.DescribeVolumesModificationsInput{}
	}

	slog.InfoContext(ctx, "DescribeVolumesModifications", "volumeIds", input.VolumeIds)

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeVolumesModificationsValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeVolumesModifications: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	var modifications []*ec2.VolumeModification

	// Fast path: specific volume IDs requested.
	if len(input.VolumeIds) > 0 {
		results := s.fetchVolumeModificationsByIDs(ctx, input.VolumeIds, accountID)
		// AWS contract: any unknown / cross-tenant ID fails the whole call.
		for i, vid := range input.VolumeIds {
			if vid == nil {
				continue
			}
			if results[i].err != nil {
				slog.ErrorContext(ctx, "DescribeVolumesModifications volume not found", "volumeId", *vid, "err", results[i].err)
				return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
			}
		}
		for _, r := range results {
			if r.modification == nil {
				continue
			}
			if len(parsedFilters) > 0 && !volumeModificationMatchesFilters(r.modification, parsedFilters) {
				continue
			}
			modifications = append(modifications, r.modification)
		}
		slog.InfoContext(ctx, "DescribeVolumesModifications completed", "count", len(modifications))
		return &ec2.DescribeVolumesModificationsOutput{VolumesModifications: modifications}, nil
	}

	// Slow path: list all volumes.
	volumeIDs, err := s.listAllVolumeIDs(ctx)
	if err != nil {
		return nil, err
	}

	var volumeIDFilterValues []string
	if parsedFilters != nil {
		volumeIDFilterValues = parsedFilters["volume-id"]
	}

	for _, volumeID := range volumeIDs {
		if len(volumeIDFilterValues) > 0 {
			if !filterutil.MatchesAny(volumeIDFilterValues, volumeID) {
				continue
			}
		}

		cfg, err := s.getVolumeConfig(ctx, volumeID)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to get volume config", "volumeId", volumeID, "err", err)
			continue
		}
		if cfg.VolumeMetadata.TenantID != accountID {
			continue
		}
		if cfg.Modification == nil {
			continue
		}

		mod := vbModificationToEC2(cfg.Modification)
		if len(parsedFilters) > 0 && !volumeModificationMatchesFilters(mod, parsedFilters) {
			continue
		}
		modifications = append(modifications, mod)
	}

	slog.InfoContext(ctx, "DescribeVolumesModifications completed", "count", len(modifications))
	return &ec2.DescribeVolumesModificationsOutput{
		VolumesModifications: modifications,
	}, nil
}

// volumeModificationResult bundles a per-ID lookup result so the fast path
// can preserve input ordering and surface errors after the parallel fan-out.
type volumeModificationResult struct {
	modification *ec2.VolumeModification
	err          error
}

// fetchVolumeModificationsByIDs reads each requested volume's config in parallel,
// returning results positionally aligned with volumeIDs. Cross-tenant volumes surface
// as InvalidVolume.NotFound.
func (s *VolumeServiceImpl) fetchVolumeModificationsByIDs(ctx context.Context, volumeIDs []*string, accountID string) []volumeModificationResult {
	results := make([]volumeModificationResult, len(volumeIDs))
	var wg sync.WaitGroup

	for i, volumeID := range volumeIDs {
		if volumeID == nil {
			continue
		}
		wg.Add(1)
		go func(idx int, volID string) {
			defer wg.Done()
			cfg, err := s.GetVolumeConfig(volID)
			if err != nil {
				results[idx] = volumeModificationResult{err: errors.New(awserrors.ErrorInvalidVolumeNotFound)}
				return
			}
			if cfg.VolumeMetadata.TenantID != accountID {
				results[idx] = volumeModificationResult{err: errors.New(awserrors.ErrorInvalidVolumeNotFound)}
				return
			}
			results[idx] = volumeModificationResult{modification: vbModificationToEC2(cfg.Modification)}
		}(i, *volumeID)
	}

	wg.Wait()
	return results
}

// listAllVolumeIDs lists all volume IDs from S3 by scanning bucket prefixes.
// It filters for vol-* prefixes and skips internal sub-volumes (EFI and cloud-init).
func (s *VolumeServiceImpl) listAllVolumeIDs(ctx context.Context) ([]string, error) {
	result, err := s.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucketName),
		Delimiter: aws.String("/"),
	})
	if err != nil {
		slog.Error("Failed to list S3 objects", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var volumeIDs []string
	for _, prefix := range result.CommonPrefixes {
		if prefix.Prefix == nil {
			continue
		}

		prefixStr := *prefix.Prefix

		if !strings.HasPrefix(prefixStr, "vol-") {
			continue
		}

		volumeID := strings.TrimSuffix(prefixStr, "/")

		// Skip internal sub-volumes: the EFI partition and the legacy cloud-init
		// seed (no longer created post boot-from-IMDS, still filtered so any
		// pre-cutover volume never surfaces as a user volume).
		if strings.HasSuffix(volumeID, "-efi") || strings.HasSuffix(volumeID, "-cloudinit") {
			continue
		}

		volumeIDs = append(volumeIDs, volumeID)
	}

	return volumeIDs, nil
}

// fetchVolumesByIDs fetches multiple volumes in parallel by their IDs,
// filtering by accountID for account scoping.
func (s *VolumeServiceImpl) fetchVolumesByIDs(ctx context.Context, volumeIDs []*string, accountID string) []*ec2.Volume {
	var (
		volumes []*ec2.Volume
		mu      sync.Mutex
		wg      sync.WaitGroup
	)

	for _, volumeID := range volumeIDs {
		if volumeID == nil {
			continue
		}

		wg.Add(1)
		go func(volID string) {
			defer wg.Done()

			result, err := s.getVolumeByID(ctx, volID)
			if err != nil {
				slog.Debug("Volume not found", "volumeId", volID, "err", err)
				return
			}

			// Skip volumes not owned by the caller's account.
			if result.tenantID != accountID {
				return
			}

			mu.Lock()
			volumes = append(volumes, result.volume)
			mu.Unlock()
		}(*volumeID)
	}

	wg.Wait()
	return volumes
}

// volumeResult bundles an ec2.Volume with the owning account's TenantID
// so callers can filter by account without a second config read.
type volumeResult struct {
	volume   *ec2.Volume
	tenantID string
}

// getVolumeByID fetches a single volume's config from S3 and builds an EC2 Volume.
// Returns the volume and the stored TenantID for account scoping.
func (s *VolumeServiceImpl) getVolumeByID(ctx context.Context, volumeID string) (*volumeResult, error) {
	cfg, encryptionEnabled, err := s.getVolumeConfigAndEncryption(ctx, volumeID)
	if err != nil {
		return nil, err
	}

	volMeta := cfg.VolumeMetadata

	if volMeta.VolumeID == "" {
		slog.Debug("Volume ID is empty in config", "key", volumeID+"/config.json")
		return nil, errors.New("volume ID is empty")
	}

	if volMeta.SizeGiB == 0 {
		slog.Error("Volume has zero size in config", "volumeId", volumeID)
		return nil, fmt.Errorf("volume %s has zero size in config", volumeID)
	}

	// An empty State is internal drift, not a valid AWS state. Derive the
	// effective state from ground truth (the attachment) rather than blindly
	// rendering "available", which would hide an empty-but-attached volume.
	state := volMeta.State
	if state == "" {
		if volMeta.AttachedInstance != "" {
			state = "in-use"
		} else {
			state = "available"
		}
	}
	volumeType := volMeta.VolumeType
	if volumeType == "" {
		volumeType = "gp3"
	}

	volume := &ec2.Volume{
		VolumeId:         aws.String(volMeta.VolumeID),
		Size:             aws.Int64(utils.SafeUint64ToInt64(volMeta.SizeGiB)),
		State:            aws.String(state),
		AvailabilityZone: aws.String(volMeta.AvailabilityZone),
		CreateTime:       aws.Time(volMeta.CreatedAt),
		VolumeType:       aws.String(volumeType),
		Encrypted:        aws.Bool(encryptionEnabled),
	}

	if volMeta.IOPS > 0 {
		volume.Iops = aws.Int64(int64(volMeta.IOPS))
	}

	if volMeta.SnapshotID != "" {
		volume.SnapshotId = aws.String(volMeta.SnapshotID)
	}

	if volMeta.AttachedInstance != "" {
		attachState := "attached"
		if volMeta.State != "in-use" {
			attachState = "detached"
		}
		volume.Attachments = []*ec2.VolumeAttachment{
			{
				VolumeId:            aws.String(volMeta.VolumeID),
				InstanceId:          aws.String(volMeta.AttachedInstance),
				Device:              aws.String(volMeta.DeviceName),
				State:               aws.String(attachState),
				DeleteOnTermination: aws.Bool(volMeta.DeleteOnTermination),
				AttachTime:          aws.Time(volMeta.AttachedAt),
			},
		}
	}

	volume.Tags = utils.MapToEC2Tags(volMeta.Tags)

	return &volumeResult{volume: volume, tenantID: volMeta.TenantID}, nil
}

// volumeConfigWrapper matches the JSON structure stored in S3 config.json files
type volumeConfigWrapper struct {
	VolumeConfig viperblock.VolumeConfig `json:"VolumeConfig"`
}

// volumeStateRecord is the control-plane-owned attachment state, persisted to a
// per-volume state.json object kept out of config.json. config.json is rewritten
// by the live nbdkit VB on every SaveState (clobbering any State the control
// plane wrote there) and is a sealed object for encrypted volumes (a second
// writer reuses the AES-GCM nonce). state.json is plaintext, viperblock never
// touches it, so the control plane is its single writer.
type volumeStateRecord struct {
	State            string    `json:"state"`
	AttachedInstance string    `json:"attachedInstance"`
	DeviceName       string    `json:"deviceName"`
	AttachedAt       time.Time `json:"attachedAt"`
}

// volumeStateKey is the S3 key for a volume's control-plane state object.
func volumeStateKey(volumeID string) string { return volumeID + "/state.json" }

// putVolumeState writes the control-plane attachment state to state.json.
func (s *VolumeServiceImpl) putVolumeState(ctx context.Context, volumeID string, rec volumeStateRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal volume state: %w", err)
	}
	_, err = s.store.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(volumeStateKey(volumeID)),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("failed to write volume state to S3: %w", err)
	}
	return nil
}

// getVolumeState reads state.json. found=false with a nil error means the object
// is absent (a volume predating the state.json split), in which case the caller
// falls back to the State embedded in config.json.
func (s *VolumeServiceImpl) getVolumeState(ctx context.Context, volumeID string) (volumeStateRecord, bool, error) {
	getResult, err := s.store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(volumeStateKey(volumeID)),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return volumeStateRecord{}, false, nil
		}
		return volumeStateRecord{}, false, fmt.Errorf("failed to get volume state: %w", err)
	}
	defer getResult.Body.Close()

	body, err := io.ReadAll(getResult.Body)
	if err != nil {
		return volumeStateRecord{}, false, fmt.Errorf("failed to read volume state body: %w", err)
	}
	var rec volumeStateRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return volumeStateRecord{}, false, fmt.Errorf("failed to unmarshal volume state: %w", err)
	}
	return rec, true, nil
}

// GetVolumeConfig reads the raw VolumeConfig from S3 for a given volume ID.
func (s *VolumeServiceImpl) GetVolumeConfig(volumeID string) (*viperblock.VolumeConfig, error) {
	return s.getVolumeConfig(context.Background(), volumeID)
}

// getVolumeConfig is GetVolumeConfig carrying the caller's context.
func (s *VolumeServiceImpl) getVolumeConfig(ctx context.Context, volumeID string) (*viperblock.VolumeConfig, error) {
	cfg, _, err := s.getVolumeConfigAndEncryption(ctx, volumeID)
	return cfg, err
}

// getVolumeConfigAndEncryption reads config.json and returns the VolumeConfig plus the
// VBState.EncryptionEnabled flag. Pre-VBState blobs report encryptionEnabled=false.
func (s *VolumeServiceImpl) getVolumeConfigAndEncryption(ctx context.Context, volumeID string) (*viperblock.VolumeConfig, bool, error) {
	configKey := volumeID + "/config.json"

	getResult, err := s.store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(configKey),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, false, errors.New(awserrors.ErrorInvalidVolumeNotFound)
		}
		return nil, false, fmt.Errorf("failed to get config: %w", err)
	}
	defer getResult.Body.Close()

	body, err := io.ReadAll(getResult.Body)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read config body: %w", err)
	}
	// Unwrap the at-rest encryption envelope (authenticated-but-plaintext
	// metadata); no-op for unencrypted volumes.
	body = viperblock.StateBody(body)

	// Try full VBState first (matches mergeVolumeConfig's careful decode
	// pattern). A populated BlockSize is the marker that the blob is a full
	// state rather than a wrapper-only fallback.
	var vc *viperblock.VolumeConfig
	var encryptionEnabled bool

	var state viperblock.VBState
	if decodeErr := json.NewDecoder(bytes.NewReader(body)).Decode(&state); decodeErr == nil && state.BlockSize != 0 {
		vc = &state.VolumeConfig
		encryptionEnabled = state.EncryptionEnabled
	} else {
		var wrapper volumeConfigWrapper
		if err := json.Unmarshal(body, &wrapper); err != nil {
			return nil, false, fmt.Errorf("failed to unmarshal config: %w", err)
		}
		vc = &wrapper.VolumeConfig
	}

	// Overlay the control-plane-owned attachment state from state.json. The State
	// embedded in config.json is rewritten by the live nbdkit VB (stale
	// "available") and is not authoritative; state.json is. Absent state.json
	// keeps the embedded value (volumes predating the split).
	if rec, found, stateErr := s.getVolumeState(ctx, volumeID); stateErr != nil {
		return nil, false, stateErr
	} else if found {
		vc.VolumeMetadata.State = rec.State
		vc.VolumeMetadata.AttachedInstance = rec.AttachedInstance
		vc.VolumeMetadata.DeviceName = rec.DeviceName
		vc.VolumeMetadata.AttachedAt = rec.AttachedAt
	}

	return vc, encryptionEnabled, nil
}

// putVolumeConfig writes a VolumeConfig back to S3 as config.json.
// It performs a read-modify-write to preserve full VBState if viperblock
// has already written state (BlockSize, SeqNum, WALNum, etc.) to config.json.
// Callers are the safe non-live writers only (CreateVolume pre-mount,
// markVolumeOrphaned detached, ModifyVolume stopped-instance); the
// control-plane attachment state of a live-mounted volume goes to state.json via
// UpdateVolumeState, never here.
func (s *VolumeServiceImpl) putVolumeConfig(ctx context.Context, volumeID string, cfg *viperblock.VolumeConfig) error {
	configKey := volumeID + "/config.json"

	// config.json for an encrypted volume is a sealed VBState whose AES-GCM tag
	// and StateSeqNum-derived nonce can only be advanced by the master-key holder.
	// With no live owner, hand it to a viperblockd worker that opens the detached
	// volume exclusively and reseals — a detached volume has no concurrent writer.
	encrypted, err := s.configIsEncrypted(ctx, volumeID)
	if err != nil {
		return err
	}
	if encrypted {
		return s.putVolumeConfigViaDetached(ctx, volumeID, cfg)
	}

	data, err := s.mergeVolumeConfig(ctx, configKey, cfg)
	if err != nil {
		return err
	}

	_, err = s.store.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(configKey),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("failed to write config to S3: %w", err)
	}

	return nil
}

// configIsEncrypted reports whether the persisted config.json for volumeID is a
// sealed (encrypted) VBState. A missing object (new volume) reports false.
func (s *VolumeServiceImpl) configIsEncrypted(ctx context.Context, volumeID string) (bool, error) {
	getResult, err := s.store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(volumeID + "/config.json"),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read config for encryption check: %w", err)
	}
	defer getResult.Body.Close()

	body, err := io.ReadAll(getResult.Body)
	if err != nil {
		return false, fmt.Errorf("failed to read config body: %w", err)
	}

	var state viperblock.VBState
	if decodeErr := json.Unmarshal(viperblock.StateBody(body), &state); decodeErr == nil && state.BlockSize != 0 {
		return state.EncryptionEnabled, nil
	}
	return false, nil
}

// putVolumeConfigViaDetached hands an encrypted-volume config update to the
// ebs.config queue group, where a viperblockd worker opens the detached volume
// exclusively and reseals. Safe only when no node owns the volume: a detached
// volume has no concurrent writer, so the reopen is nonce-safe.
func (s *VolumeServiceImpl) putVolumeConfigViaDetached(ctx context.Context, volumeID string, cfg *viperblock.VolumeConfig) error {
	if s.natsConn == nil {
		return fmt.Errorf("encrypted volume %s requires NATS to reach the viperblock keyholder, but no connection is configured", volumeID)
	}
	cfgData, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal VolumeConfig: %w", err)
	}
	req := types.EBSConfigUpdateRequest{Volume: volumeID, VolumeConfig: cfgData}
	resp, err := utils.NATSRequestCtx[types.EBSConfigUpdateResponse](ctx, s.natsConn, "ebs.config", req, 30*time.Second, "")
	if err != nil {
		return fmt.Errorf("ebs.config request for %s: %w", volumeID, err)
	}
	if !resp.Success || resp.Error != "" {
		return fmt.Errorf("ebs.config update for %s failed: %s", volumeID, resp.Error)
	}
	return nil
}

// mergeVolumeConfig reads existing config.json from S3 and merges the new VolumeConfig
// into it, preserving full VBState when present. Refuses to merge encrypted VBState —
// re-marshaling without the master key would corrupt the on-disk AES-GCM tag.
func (s *VolumeServiceImpl) mergeVolumeConfig(ctx context.Context, configKey string, cfg *viperblock.VolumeConfig) ([]byte, error) {
	getResult, err := s.store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(configKey),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			// No existing config.json -- write wrapper for new volume
			return json.Marshal(volumeConfigWrapper{VolumeConfig: *cfg})
		}
		return nil, fmt.Errorf("failed to read existing config for merge: %w", err)
	}
	defer getResult.Body.Close()

	body, err := io.ReadAll(getResult.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read existing config: %w", err)
	}

	// StateBody unwraps the at-rest encryption envelope so the guard below sees
	// the real VBState. Without it the wrapper decodes to a zero-valued state
	// (BlockSize==0), routing to the wrapper-only path and bricking the volume.
	var state viperblock.VBState
	if decodeErr := json.Unmarshal(viperblock.StateBody(body), &state); decodeErr != nil || state.BlockSize == 0 {
		// Not a full VBState (new volume or wrapper-only) -- write wrapper
		return json.Marshal(volumeConfigWrapper{VolumeConfig: *cfg})
	}

	if state.EncryptionEnabled {
		return nil, fmt.Errorf("mergeVolumeConfig: refusing to merge encrypted VBState for %s without master key (would strip AES-GCM tag and brick volume)", configKey)
	}

	// Full VBState exists -- update VolumeConfig and reconcile VolumeSize
	state.VolumeConfig = *cfg
	configSizeBytes := cfg.VolumeMetadata.SizeGiB * 1024 * 1024 * 1024
	if configSizeBytes > 0 && configSizeBytes > state.VolumeSize {
		state.VolumeSize = configSizeBytes
	}

	slog.Info("putVolumeConfig: preserved VBState", "volumeId", strings.TrimSuffix(configKey, "/config.json"),
		"blockSize", state.BlockSize, "seqNum", state.SeqNum)

	return json.Marshal(state)
}

// UpdateVolumeState updates the control-plane-owned attachment state (state,
// attachment, device) by writing the per-volume state.json. It does NOT write
// config.json: the live nbdkit VB owns that object and its next SaveState
// clobbers any State written out-of-band (and for an encrypted volume a second
// writer reuses the AES-GCM nonce). Readers overlay state.json in
// getVolumeConfigAndEncryption. The config.json read here is a presence/ownership
// gate so a missing volume still errors.
func (s *VolumeServiceImpl) UpdateVolumeState(volumeID, state, attachedInstance, deviceName string) error {
	if _, err := s.GetVolumeConfig(volumeID); err != nil {
		return fmt.Errorf("failed to get volume config for state update: %w", err)
	}

	// A detached volume is "available": never persist an empty State for an
	// unattached volume, so a detach/terminate writeback that omits the state
	// cannot strand the volume in drift that later reads as undeletable.
	if state == "" && attachedInstance == "" {
		state = "available"
	}

	rec := volumeStateRecord{
		State:            state,
		AttachedInstance: attachedInstance,
		DeviceName:       deviceName,
	}
	if attachedInstance != "" {
		rec.AttachedAt = time.Now()
	}

	if err := s.putVolumeState(context.Background(), volumeID, rec); err != nil {
		return fmt.Errorf("failed to write volume state: %w", err)
	}

	slog.Info("Updated volume state", "volumeId", volumeID, "state", state, "attachedInstance", attachedInstance, "deviceName", deviceName)
	return nil
}

// ModifyVolume modifies an EBS volume (grow-only, requires stopped instance)
func (s *VolumeServiceImpl) ModifyVolume(ctx context.Context, input *ec2.ModifyVolumeInput, accountID string) (*ec2.ModifyVolumeOutput, error) {
	if input.VolumeId == nil || *input.VolumeId == "" {
		return nil, errors.New(awserrors.ErrorInvalidVolumeIDMalformed)
	}

	volumeID := *input.VolumeId
	slog.InfoContext(ctx, "ModifyVolume request", "volumeId", volumeID)

	cfg, err := s.getVolumeConfig(ctx, volumeID)
	if err != nil {
		slog.ErrorContext(ctx, "ModifyVolume failed to get volume config", "volumeId", volumeID, "err", err)
		return nil, err
	}

	// Verify caller owns this volume
	if cfg.VolumeMetadata.TenantID != accountID {
		return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}

	volMeta := &cfg.VolumeMetadata

	// Record original values before modification
	originalSize := utils.SafeUint64ToInt64(volMeta.SizeGiB)
	originalType := volMeta.VolumeType
	if originalType == "" {
		originalType = "gp3"
	}
	originalIOPS := int64(volMeta.IOPS)

	// Validate: grow only (new size must be greater than current)
	if input.Size != nil && *input.Size <= originalSize {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Validate: if volume is attached, instance must not be in-use (must be stopped)
	if volMeta.AttachedInstance != "" && volMeta.State == "in-use" {
		return nil, errors.New(awserrors.ErrorIncorrectState)
	}

	// Apply modifications
	if input.Size != nil {
		volMeta.SizeGiB = utils.SafeInt64ToUint64(*input.Size)
	}
	if input.VolumeType != nil {
		volMeta.VolumeType = *input.VolumeType
	}
	if input.Iops != nil {
		volMeta.IOPS = int(*input.Iops)
	}

	// Build target values (after modification)
	targetSize := utils.SafeUint64ToInt64(volMeta.SizeGiB)
	targetType := volMeta.VolumeType
	if targetType == "" {
		targetType = "gp3"
	}
	targetIOPS := int64(volMeta.IOPS)

	// Persist the modification record so DescribeVolumesModifications can read it back.
	// Modifications are synchronous, so state is always completed/100.
	now := time.Now()
	cfg.Modification = &viperblock.VolumeModification{
		VolumeID:           volumeID,
		ModificationState:  "completed",
		Progress:           100,
		OriginalSize:       originalSize,
		OriginalIops:       originalIOPS,
		OriginalVolumeType: originalType,
		TargetSize:         targetSize,
		TargetIops:         targetIOPS,
		TargetVolumeType:   targetType,
		StartTime:          now,
		EndTime:            now,
	}

	// Persist updated config
	if err := s.putVolumeConfig(ctx, volumeID, cfg); err != nil {
		slog.ErrorContext(ctx, "ModifyVolume failed to write config", "volumeId", volumeID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	modification := vbModificationToEC2(cfg.Modification)

	slog.InfoContext(ctx, "ModifyVolume completed", "volumeId", volumeID,
		"originalSize", originalSize, "targetSize", targetSize)

	return &ec2.ModifyVolumeOutput{
		VolumeModification: modification,
	}, nil
}

// DeleteVolume deletes an EBS volume: validates state, notifies viperblockd, and removes S3 data
func (s *VolumeServiceImpl) DeleteVolume(ctx context.Context, input *ec2.DeleteVolumeInput, accountID string) (*ec2.DeleteVolumeOutput, error) {
	if input == nil || input.VolumeId == nil || *input.VolumeId == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	volumeID := *input.VolumeId
	slog.InfoContext(ctx, "DeleteVolume request", "volumeId", volumeID)

	// Fetch volume config to validate state. AWS-faithful: an absent volume
	// returns InvalidVolume.NotFound (the provider tolerates it on destroy);
	// destroy orchestration tolerates it too.
	cfg, err := s.getVolumeConfig(ctx, volumeID)
	if err != nil {
		slog.ErrorContext(ctx, "DeleteVolume failed to get volume config", "volumeId", volumeID, "err", err)
		return nil, err
	}

	// Verify caller owns this volume
	if cfg.VolumeMetadata.TenantID != accountID {
		return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}

	// Validate: an unattached volume is deletable. State must be "available" OR
	// empty: a detach/terminate that failed to write back "available" leaves the
	// State drifted to empty with no attachment, and gating on State=="available"
	// exactly would return VolumeInUse for a volume nothing is using, stranding it
	// undeletable and blocking stack teardown.
	state := cfg.VolumeMetadata.State
	if cfg.VolumeMetadata.AttachedInstance != "" || (state != "available" && state != "") {
		slog.ErrorContext(ctx, "DeleteVolume: volume is in use", "volumeId", volumeID, "state", state, "attachedInstance", cfg.VolumeMetadata.AttachedInstance)
		return nil, errors.New(awserrors.ErrorVolumeInUse)
	}

	// Check if any snapshots reference this volume via JetStream KV.
	// Snapshot-backed clones read chunk files from the source volume's
	// S3 prefix via ReadFrom(). Deleting the source would break all clones.
	if err := s.checkVolumeHasNoSnapshots(volumeID); err != nil {
		return nil, err
	}

	// Notify viperblockd to stop nbdkit/WAL syncer (best-effort)
	if s.natsConn != nil {
		deleteReq := types.EBSDeleteRequest{Volume: volumeID}
		deleteResp, err := utils.NATSRequestCtx[types.EBSDeleteResponse](ctx, s.natsConn, "ebs.delete", deleteReq, 5*time.Second, accountID)
		if err != nil {
			slog.WarnContext(ctx, "ebs.delete notification failed (volume may not be mounted)", "volumeId", volumeID, "err", err)
		} else if deleteResp.Error != "" {
			slog.ErrorContext(ctx, "ebs.delete returned error", "volumeId", volumeID, "err", deleteResp.Error)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	} else {
		slog.WarnContext(ctx, "DeleteVolume: natsConn is nil, skipping viperblockd notification", "volumeId", volumeID)
	}

	// Delete all S3 objects for this volume and its sub-volumes.
	// Auxiliary prefixes are deleted first so the main config.json remains
	// available for retry if an auxiliary deletion fails.
	prefixes := []string{
		volumeID + "-efi/",
		volumeID + "/",
	}

	for _, prefix := range prefixes {
		if err := s.deleteS3Prefix(ctx, prefix); err != nil {
			slog.ErrorContext(ctx, "DeleteVolume failed to delete S3 prefix", "prefix", prefix, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	slog.InfoContext(ctx, "DeleteVolume completed", "volumeId", volumeID)

	return &ec2.DeleteVolumeOutput{}, nil
}

// deleteS3Prefix deletes all S3 objects under the given prefix
func (s *VolumeServiceImpl) deleteS3Prefix(ctx context.Context, prefix string) error {
	bucket := s.bucketName

	var continuationToken *string
	for {
		listOutput, err := s.store.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return fmt.Errorf("failed to list objects with prefix %s: %w", prefix, err)
		}

		if len(listOutput.Contents) == 0 {
			break
		}

		for _, obj := range listOutput.Contents {
			_, err := s.store.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucket),
				Key:    obj.Key,
			})
			if err != nil {
				return fmt.Errorf("failed to delete object %s: %w", *obj.Key, err)
			}
		}

		if !aws.BoolValue(listOutput.IsTruncated) {
			break
		}
		continuationToken = listOutput.NextContinuationToken
	}

	return nil
}

// snapshotMetadata holds the subset of snapshot metadata needed by CreateVolume.
// Matches the JSON written by the snapshot service's SnapshotConfig.
type snapshotMetadata struct {
	VolumeID   string `json:"volume_id"`
	VolumeSize int64  `json:"volume_size"`
}

// getSnapshotMetadata reads snapshot metadata.json from S3 for CreateVolume.
func (s *VolumeServiceImpl) getSnapshotMetadata(ctx context.Context, snapshotID string) (*snapshotMetadata, error) {
	key := snapshotID + "/metadata.json"

	getResult, err := s.store.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
		}
		return nil, fmt.Errorf("failed to get snapshot metadata: %w", err)
	}
	defer getResult.Body.Close()

	body, err := io.ReadAll(getResult.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read snapshot metadata: %w", err)
	}

	var meta snapshotMetadata
	if err := json.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("failed to decode snapshot metadata: %w", err)
	}

	return &meta, nil
}

// checkVolumeHasNoSnapshots checks if a volume has dependent snapshots
// using the JetStream KV index.
func (s *VolumeServiceImpl) checkVolumeHasNoSnapshots(volumeID string) error {
	if s.snapshotKV == nil {
		slog.Error("checkVolumeHasNoSnapshots: snapshotKV is nil", "volumeId", volumeID)
		return errors.New(awserrors.ErrorServerInternal)
	}

	has, err := s.volumeHasSnapshotsKV(volumeID)
	if err != nil {
		slog.Error("checkVolumeHasNoSnapshots: KV lookup failed", "volumeId", volumeID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if has {
		slog.Error("DeleteVolume blocked: volume has snapshots", "volumeId", volumeID)
		return errors.New(awserrors.ErrorVolumeInUse)
	}
	return nil
}

// volumeHasSnapshotsKV checks the JetStream KV index for snapshot references.
func (s *VolumeServiceImpl) volumeHasSnapshotsKV(volumeID string) (bool, error) {
	entry, err := s.snapshotKV.Get(volumeID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return false, nil
		}
		return false, err
	}

	var snapshots []string
	if err := json.Unmarshal(entry.Value(), &snapshots); err != nil {
		return false, err
	}

	return len(snapshots) > 0, nil
}

// ApplyRecordTags mirrors CreateTags into the owning volume config so
// DescribeVolumes observes tags added after create. Non-vol ids, volumes
// absent from this store, and volumes the caller does not own are skipped.
func (s *VolumeServiceImpl) ApplyRecordTags(input *ec2.CreateTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return s.mirrorVolumeTags(context.Background(), input.Resources, accountID, utils.MergeTagsMut(input))
}

// RemoveRecordTags mirrors DeleteTags into the owning volume config with
// AWS-faithful delete semantics.
func (s *VolumeServiceImpl) RemoveRecordTags(input *ec2.DeleteTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return s.mirrorVolumeTags(context.Background(), input.Resources, accountID, utils.RemoveTagsMut(input))
}

// mirrorVolumeTags read-modify-writes VolumeMetadata.Tags for each vol- id.
// config.json lives at a global ID-keyed path, so the mutation is gated on the
// caller owning the volume (TenantID match); mismatch or absence is a no-op.
func (s *VolumeServiceImpl) mirrorVolumeTags(ctx context.Context, resources []*string, accountID string, mut func(map[string]string)) error {
	for _, res := range resources {
		if res == nil || !strings.HasPrefix(*res, "vol-") {
			continue
		}
		cfg, err := s.GetVolumeConfig(*res)
		if err != nil {
			if err.Error() == awserrors.ErrorInvalidVolumeNotFound {
				continue
			}
			return err
		}
		if cfg.VolumeMetadata.TenantID != accountID {
			slog.Debug("mirrorVolumeTags: skipping volume not owned by caller", "volumeId", *res)
			continue
		}
		if cfg.VolumeMetadata.Tags == nil {
			cfg.VolumeMetadata.Tags = map[string]string{}
		}
		mut(cfg.VolumeMetadata.Tags)
		if err := s.putVolumeConfig(ctx, *res, cfg); err != nil {
			return err
		}
	}
	return nil
}
