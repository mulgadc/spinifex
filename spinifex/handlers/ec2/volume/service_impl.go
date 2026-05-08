package handlers_ec2_volume

import (
	"bytes"
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

const defaultGP3IOPS = 3000

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
func (s *VolumeServiceImpl) CreateVolume(input *ec2.CreateVolumeInput, accountID string) (*ec2.Volume, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Validate volume type: only gp3 supported (or empty defaults to gp3)
	if input.VolumeType != nil && *input.VolumeType != "" && *input.VolumeType != "gp3" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
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
		snapMeta, err := s.getSnapshotMetadata(snapshotID)
		if err != nil {
			slog.Error("CreateVolume: snapshot not found", "snapshotId", snapshotID, "err", err)
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
			slog.Error("CreateVolume: requested size smaller than snapshot", "size", *input.Size, "snapshotSize", snapshotSizeGiB)
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

	iops := defaultGP3IOPS

	slog.Info("CreateVolume", "volumeId", volumeID, "size", size, "type", volumeType,
		"az", *input.AvailabilityZone, "snapshotId", snapshotID)

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
			IsEncrypted:      false,
			SnapshotID:       snapshotID,
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

	vbconfig := viperblock.VB{
		VolumeName:   volumeID,
		VolumeSize:   volumeSizeBytes,
		BaseDir:      s.config.WalDir,
		Cache:        viperblock.Cache{Config: viperblock.CacheConfig{Size: 0}},
		VolumeConfig: volumeConfig,
	}

	// If created from a snapshot, set the snapshot fields so viperblock's
	// LoadState will call OpenFromSnapshot to load the base block map.
	if snapshotID != "" {
		vbconfig.SnapshotID = snapshotID
		vbconfig.SourceVolumeName = sourceVolumeName
	}

	vb, err := viperblock.New(&vbconfig, "s3", cfg)
	if err != nil {
		slog.Error("CreateVolume failed to create viperblock instance", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	vb.SetDebug(false)

	// Initialize the backend (creates bucket structure in S3)
	if err := vb.Backend.Init(); err != nil {
		slog.Error("CreateVolume failed to initialize backend", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Persist volume state to S3 (writes config.json)
	if err := vb.SaveState(); err != nil {
		slog.Error("CreateVolume failed to save state", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CreateVolume completed", "volumeId", volumeID, "size", size, "type", volumeType)

	vol := &ec2.Volume{
		VolumeId:         aws.String(volumeID),
		Size:             aws.Int64(size),
		VolumeType:       aws.String(volumeType),
		State:            aws.String("available"),
		AvailabilityZone: input.AvailabilityZone,
		CreateTime:       aws.Time(now),
		Iops:             aws.Int64(int64(iops)),
		Encrypted:        aws.Bool(false),
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
func (s *VolumeServiceImpl) DescribeVolumes(input *ec2.DescribeVolumesInput, accountID string) (*ec2.DescribeVolumesOutput, error) {
	if input == nil {
		input = &ec2.DescribeVolumesInput{}
	}

	slog.Info("Describing volumes", "volumeIds", input.VolumeIds)

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeVolumesValidFilters)
	if err != nil {
		slog.Warn("DescribeVolumes: invalid filter", "err", err)
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
		volumes = s.fetchVolumesByIDs(input.VolumeIds, accountID)
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

		slog.Info("DescribeVolumes completed", "count", len(volumes))
		return &ec2.DescribeVolumesOutput{Volumes: volumes}, nil
	}

	// Slow path: list all volumes (no specific IDs requested)
	volumeIDs, err := s.listAllVolumeIDs()
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

		result, err := s.getVolumeByID(volumeID)
		if err != nil {
			slog.Error("Failed to get volume", "volumeId", volumeID, "err", err)
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

	slog.Info("DescribeVolumes completed", "count", len(volumes))

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

func (s *VolumeServiceImpl) DescribeVolumeStatus(input *ec2.DescribeVolumeStatusInput, accountID string) (*ec2.DescribeVolumeStatusOutput, error) {
	if input == nil {
		input = &ec2.DescribeVolumeStatusInput{}
	}

	slog.Info("DescribeVolumeStatus", "volumeIds", input.VolumeIds)

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeVolumeStatusValidFilters)
	if err != nil {
		slog.Warn("DescribeVolumeStatus: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	var statusItems []*ec2.VolumeStatusItem

	// Fast path: if specific volume IDs are requested, fetch them directly
	if len(input.VolumeIds) > 0 {
		for _, vid := range input.VolumeIds {
			if vid == nil {
				continue
			}
			item, tenantID, err := s.getVolumeStatusByID(*vid)
			if err != nil {
				slog.Error("DescribeVolumeStatus volume not found", "volumeId", *vid, "err", err)
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
		slog.Info("DescribeVolumeStatus completed", "count", len(statusItems))
		return &ec2.DescribeVolumeStatusOutput{VolumeStatuses: statusItems}, nil
	}

	// Slow path: list all volumes (no specific IDs requested)
	volumeIDs, err := s.listAllVolumeIDs()
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

		item, tenantID, err := s.getVolumeStatusByID(volumeID)
		if err != nil {
			slog.Error("Failed to get volume status", "volumeId", volumeID, "err", err)
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

	slog.Info("DescribeVolumeStatus completed", "count", len(statusItems))

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
func (s *VolumeServiceImpl) getVolumeStatusByID(volumeID string) (*ec2.VolumeStatusItem, string, error) {
	result, err := s.getVolumeByID(volumeID)
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
func (s *VolumeServiceImpl) DescribeVolumesModifications(input *ec2.DescribeVolumesModificationsInput, accountID string) (*ec2.DescribeVolumesModificationsOutput, error) {
	if input == nil {
		input = &ec2.DescribeVolumesModificationsInput{}
	}

	slog.Info("DescribeVolumesModifications", "volumeIds", input.VolumeIds)

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeVolumesModificationsValidFilters)
	if err != nil {
		slog.Warn("DescribeVolumesModifications: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	var modifications []*ec2.VolumeModification

	// Fast path: specific volume IDs requested.
	if len(input.VolumeIds) > 0 {
		results := s.fetchVolumeModificationsByIDs(input.VolumeIds, accountID)
		// AWS contract: any unknown / cross-tenant ID fails the whole call.
		for i, vid := range input.VolumeIds {
			if vid == nil {
				continue
			}
			if results[i].err != nil {
				slog.Error("DescribeVolumesModifications volume not found", "volumeId", *vid, "err", results[i].err)
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
		slog.Info("DescribeVolumesModifications completed", "count", len(modifications))
		return &ec2.DescribeVolumesModificationsOutput{VolumesModifications: modifications}, nil
	}

	// Slow path: list all volumes.
	volumeIDs, err := s.listAllVolumeIDs()
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

		cfg, err := s.GetVolumeConfig(volumeID)
		if err != nil {
			slog.Error("Failed to get volume config", "volumeId", volumeID, "err", err)
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

	slog.Info("DescribeVolumesModifications completed", "count", len(modifications))
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

// fetchVolumeModificationsByIDs reads each requested volume's config in
// parallel, returning a result slice positionally aligned with volumeIDs.
// Cross-tenant volumes surface as InvalidVolume.NotFound, mirroring
// DescribeVolumes' silent tenant scoping.
func (s *VolumeServiceImpl) fetchVolumeModificationsByIDs(volumeIDs []*string, accountID string) []volumeModificationResult {
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
func (s *VolumeServiceImpl) listAllVolumeIDs() ([]string, error) {
	result, err := s.store.ListObjectsV2(&s3.ListObjectsV2Input{
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

		// Skip internal sub-volumes (EFI and cloud-init partitions)
		if strings.HasSuffix(volumeID, "-efi") || strings.HasSuffix(volumeID, "-cloudinit") {
			continue
		}

		volumeIDs = append(volumeIDs, volumeID)
	}

	return volumeIDs, nil
}

// fetchVolumesByIDs fetches multiple volumes in parallel by their IDs,
// filtering by accountID for account scoping.
func (s *VolumeServiceImpl) fetchVolumesByIDs(volumeIDs []*string, accountID string) []*ec2.Volume {
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

			result, err := s.getVolumeByID(volID)
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
func (s *VolumeServiceImpl) getVolumeByID(volumeID string) (*volumeResult, error) {
	cfg, err := s.GetVolumeConfig(volumeID)
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

	state := volMeta.State
	if state == "" {
		state = "available"
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
		Encrypted:        aws.Bool(volMeta.IsEncrypted),
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

// GetVolumeConfig reads the raw VolumeConfig from S3 for a given volume ID.
func (s *VolumeServiceImpl) GetVolumeConfig(volumeID string) (*viperblock.VolumeConfig, error) {
	configKey := volumeID + "/config.json"

	getResult, err := s.store.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(configKey),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
		}
		return nil, fmt.Errorf("failed to get config: %w", err)
	}
	defer getResult.Body.Close()

	body, err := io.ReadAll(getResult.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read config body: %w", err)
	}

	var wrapper volumeConfigWrapper
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return &wrapper.VolumeConfig, nil
}

// putVolumeConfig writes a VolumeConfig back to S3 as config.json.
// It performs a read-modify-write to preserve full VBState if viperblock
// has already written state (BlockSize, SeqNum, WALNum, etc.) to config.json.
func (s *VolumeServiceImpl) putVolumeConfig(volumeID string, cfg *viperblock.VolumeConfig) error {
	configKey := volumeID + "/config.json"

	data, err := s.mergeVolumeConfig(configKey, cfg)
	if err != nil {
		return err
	}

	_, err = s.store.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(configKey),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return fmt.Errorf("failed to write config to S3: %w", err)
	}

	return nil
}

// mergeVolumeConfig reads existing config.json from S3 and merges the new
// VolumeConfig into it, preserving full VBState when present. If no existing
// VBState is found, it returns a plain volumeConfigWrapper.
func (s *VolumeServiceImpl) mergeVolumeConfig(configKey string, cfg *viperblock.VolumeConfig) ([]byte, error) {
	getResult, err := s.store.GetObject(&s3.GetObjectInput{
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

	var state viperblock.VBState
	if json.Unmarshal(body, &state) != nil || state.BlockSize == 0 {
		// Not a full VBState (new volume or wrapper-only) -- write wrapper
		return json.Marshal(volumeConfigWrapper{VolumeConfig: *cfg})
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

// UpdateVolumeState updates volume metadata (state, attachment, device) in the object store.
func (s *VolumeServiceImpl) UpdateVolumeState(volumeID, state, attachedInstance, deviceName string) error {
	cfg, err := s.GetVolumeConfig(volumeID)
	if err != nil {
		return fmt.Errorf("failed to get volume config for state update: %w", err)
	}

	cfg.VolumeMetadata.State = state
	cfg.VolumeMetadata.AttachedInstance = attachedInstance
	cfg.VolumeMetadata.DeviceName = deviceName
	if attachedInstance != "" {
		cfg.VolumeMetadata.AttachedAt = time.Now()
	}

	if err := s.putVolumeConfig(volumeID, cfg); err != nil {
		return fmt.Errorf("failed to write volume config for state update: %w", err)
	}

	slog.Info("Updated volume state", "volumeId", volumeID, "state", state, "attachedInstance", attachedInstance, "deviceName", deviceName)
	return nil
}

// ModifyVolume modifies an EBS volume (grow-only, requires stopped instance)
func (s *VolumeServiceImpl) ModifyVolume(input *ec2.ModifyVolumeInput, accountID string) (*ec2.ModifyVolumeOutput, error) {
	if input.VolumeId == nil || *input.VolumeId == "" {
		return nil, errors.New(awserrors.ErrorInvalidVolumeIDMalformed)
	}

	volumeID := *input.VolumeId
	slog.Info("ModifyVolume request", "volumeId", volumeID)

	cfg, err := s.GetVolumeConfig(volumeID)
	if err != nil {
		slog.Error("ModifyVolume failed to get volume config", "volumeId", volumeID, "err", err)
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

	// Persist the modification record alongside the volume metadata so
	// DescribeVolumesModifications can read it back. spinifex applies
	// modifications synchronously, so the persisted state is always
	// completed/100/EndTime==StartTime.
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
	if err := s.putVolumeConfig(volumeID, cfg); err != nil {
		slog.Error("ModifyVolume failed to write config", "volumeId", volumeID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	modification := vbModificationToEC2(cfg.Modification)

	slog.Info("ModifyVolume completed", "volumeId", volumeID,
		"originalSize", originalSize, "targetSize", targetSize)

	return &ec2.ModifyVolumeOutput{
		VolumeModification: modification,
	}, nil
}

// DeleteVolume deletes an EBS volume: validates state, notifies viperblockd, and removes S3 data
func (s *VolumeServiceImpl) DeleteVolume(input *ec2.DeleteVolumeInput, accountID string) (*ec2.DeleteVolumeOutput, error) {
	if input == nil || input.VolumeId == nil || *input.VolumeId == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	volumeID := *input.VolumeId
	slog.Info("DeleteVolume request", "volumeId", volumeID)

	// Fetch volume config to validate state
	cfg, err := s.GetVolumeConfig(volumeID)
	if err != nil {
		slog.Error("DeleteVolume failed to get volume config", "volumeId", volumeID, "err", err)
		return nil, err
	}

	// Verify caller owns this volume
	if cfg.VolumeMetadata.TenantID != accountID {
		return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}

	// Validate: volume must be available and not attached
	if cfg.VolumeMetadata.State != "available" || cfg.VolumeMetadata.AttachedInstance != "" {
		slog.Error("DeleteVolume: volume is in use", "volumeId", volumeID, "state", cfg.VolumeMetadata.State, "attachedInstance", cfg.VolumeMetadata.AttachedInstance)
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
		deleteData, err := json.Marshal(deleteReq)
		if err != nil {
			slog.Error("DeleteVolume failed to marshal ebs.delete request", "volumeId", volumeID, "err", err)
		} else {
			msg, err := s.natsConn.Request("ebs.delete", deleteData, 5*time.Second)
			if err != nil {
				slog.Warn("ebs.delete notification failed (volume may not be mounted)", "volumeId", volumeID, "err", err)
			} else {
				var deleteResp types.EBSDeleteResponse
				if json.Unmarshal(msg.Data, &deleteResp) == nil && deleteResp.Error != "" {
					slog.Error("ebs.delete returned error", "volumeId", volumeID, "err", deleteResp.Error)
					return nil, errors.New(awserrors.ErrorServerInternal)
				}
			}
		}
	} else {
		slog.Warn("DeleteVolume: natsConn is nil, skipping viperblockd notification", "volumeId", volumeID)
	}

	// Delete all S3 objects for this volume and its sub-volumes.
	// Auxiliary prefixes are deleted first so the main config.json remains
	// available for retry if an auxiliary deletion fails.
	prefixes := []string{
		volumeID + "-efi/",
		volumeID + "-cloudinit/",
		volumeID + "/",
	}

	for _, prefix := range prefixes {
		if err := s.deleteS3Prefix(prefix); err != nil {
			slog.Error("DeleteVolume failed to delete S3 prefix", "prefix", prefix, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	slog.Info("DeleteVolume completed", "volumeId", volumeID)

	return &ec2.DeleteVolumeOutput{}, nil
}

// deleteS3Prefix deletes all S3 objects under the given prefix
func (s *VolumeServiceImpl) deleteS3Prefix(prefix string) error {
	bucket := s.bucketName

	var continuationToken *string
	for {
		listOutput, err := s.store.ListObjectsV2(&s3.ListObjectsV2Input{
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
			_, err := s.store.DeleteObject(&s3.DeleteObjectInput{
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
func (s *VolumeServiceImpl) getSnapshotMetadata(snapshotID string) (*snapshotMetadata, error) {
	key := snapshotID + "/metadata.json"

	getResult, err := s.store.GetObject(&s3.GetObjectInput{
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
