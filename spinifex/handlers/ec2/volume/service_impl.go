package handlers_ec2_volume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/bluebottle/pkg/safecast"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Ensure VolumeServiceImpl implements VolumeService.
var _ VolumeService = (*VolumeServiceImpl)(nil)

// Ensure VolumeServiceImpl satisfies vm.VolumeStateUpdater so the manager
// can call UpdateVolumeState directly without a daemon-side adapter.
var _ vm.VolumeStateUpdater = (*VolumeServiceImpl)(nil)

// Ensure VolumeServiceImpl satisfies handlers/ec2/instance's VolumeDeleter so
// InstanceServiceImpl can call DeleteVolume/DeleteVolumeOnTerminate through
// the dependency wired via SetTerminationDeps.
var _ handlers_ec2_instance.VolumeDeleter = (*VolumeServiceImpl)(nil)

// VolumeServiceImpl handles EBS volume operations with S3 storage.
type VolumeServiceImpl struct {
	config     *config.Config
	store      objectstore.ObjectStore
	bucketName string
	natsConn   *nats.Conn
	snapshotKV jetstream.KeyValue
	provider   ebsprovider.EBSProvider
	metadata   *ebsmetadata.Store
}

// SetEBSProvider injects the provider boundary used by the control plane. It
// is set once by the composition root; a service without it can serve reads
// from ebsmetadata but cannot allocate, expand or destroy storage.
func (s *VolumeServiceImpl) SetEBSProvider(provider ebsprovider.EBSProvider) {
	s.provider = provider
}

// EBSProvider returns the injected provider boundary. Primarily for
// composition-root tests to observe wiring.
func (s *VolumeServiceImpl) EBSProvider() ebsprovider.EBSProvider {
	return s.provider
}

// MetadataStore returns the control-plane metadata store. Primarily for
// composition-root tests to observe wiring.
func (s *VolumeServiceImpl) MetadataStore() *ebsmetadata.Store {
	return s.metadata
}

// NewVolumeServiceImpl creates a new daemon-side volume service.
// snapshotKV is optional — when non-nil, DeleteVolume uses O(1) KV lookup
// instead of scanning all snapshots in S3.
func NewVolumeServiceImpl(cfg *config.Config, natsConn *nats.Conn, snapshotKV jetstream.KeyValue) *VolumeServiceImpl {
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
		metadata:   ebsmetadata.NewStore(store, cfg.Predastore.Bucket),
	}
}

// NewVolumeServiceImplWithStore creates a volume service with a custom ObjectStore (for testing).
func NewVolumeServiceImplWithStore(cfg *config.Config, store objectstore.ObjectStore, natsConn *nats.Conn, snapshotKV ...jetstream.KeyValue) *VolumeServiceImpl {
	bucketName := ""
	if cfg != nil {
		bucketName = cfg.Predastore.Bucket
	}
	svc := &VolumeServiceImpl{
		config:     cfg,
		store:      store,
		bucketName: bucketName,
		natsConn:   natsConn,
		metadata:   ebsmetadata.NewStore(store, bucketName),
	}
	if len(snapshotKV) > 0 {
		svc.snapshotKV = snapshotKV[0]
	}
	return svc
}

// CreateVolume allocates a new EBS volume through the EBS provider and records
// its control-plane metadata document.
func (s *VolumeServiceImpl) CreateVolume(ctx context.Context, input *ec2.CreateVolumeInput, accountID string) (*ec2.Volume, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// The owning account is a key segment, so an untenanted volume has nowhere
	// to be written. Refuse at the boundary rather than in the key builder.
	if accountID == "" {
		return nil, errors.New(awserrors.ErrorAuthFailure)
	}

	// Validate volume type: only gp3 supported (or empty defaults to gp3)
	if input.VolumeType != nil && *input.VolumeType != "" && *input.VolumeType != types.VolumeTypeGP3 {
		return nil, errors.New(awserrors.ErrorUnknownVolumeType)
	}
	volumeType := types.VolumeTypeGP3

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
		// The ownership check is the key: a snapshot outside the caller's prefix
		// is not found, so the endpoint never confirms another account's IDs.
		snapMeta, err := s.metadata.GetSnapshot(ctx, accountID, snapshotID)
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
	iops := types.DefaultGP3IOPS
	if input.Iops != nil {
		iops = int(*input.Iops)
	}
	maxIOPS := min(max(int(size)*types.GP3IOPSPerGiB, types.DefaultGP3IOPS), types.MaxGP3IOPS)
	if iops < types.DefaultGP3IOPS || iops > maxIOPS {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Honor caller-supplied Throughput for gp3, else the 125 MiB/s baseline.
	// Range is flat (125-1000), unlike Iops it does not scale with size.
	throughput := types.DefaultGP3Throughput
	if input.Throughput != nil {
		throughput = int(*input.Throughput)
	}
	if throughput < types.DefaultGP3Throughput || throughput > types.MaxGP3Throughput {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	slog.InfoContext(ctx, "CreateVolume", "volumeId", volumeID, "size", size, "type", volumeType,
		"az", *input.AvailabilityZone, "snapshotId", snapshotID)

	tags := utils.ExtractTags(input.TagSpecifications, "volume")

	if err := s.requireProvider(ctx, "CreateVolume"); err != nil {
		return nil, err
	}

	// SourceVolumeName must travel with the snapshot ID: a clone resolves its
	// base blocks against the source volume's prefix, and the provider rejects
	// a snapshot source without it.
	created, err := s.provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:              ebsprovider.NewVersioned(),
		VolumeID:               volumeID,
		CapacityRange:          ebsprovider.CapacityRange{RequiredBytes: size * 1024 * 1024 * 1024},
		AvailabilityZone:       *input.AvailabilityZone,
		SourceSnapshotID:       snapshotID,
		SourceSnapshotVolumeID: sourceVolumeName,
	})
	if err != nil {
		slog.ErrorContext(ctx, "CreateVolume: provider allocation failed", "volumeId", volumeID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if created == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// The control plane cannot see how a provider encrypts its volumes, but this
	// shared config knob is the one the provider resolves the same way.
	mkey, err := utils.LoadViperblockMasterKey(s.config.Viperblock.EncryptionKeyFile)
	if err != nil {
		slog.ErrorContext(ctx, "CreateVolume: failed to load encryption key", "volumeId", volumeID, "err", err)
		_ = s.provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, Handle: created.Handle})
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if err := s.metadata.PutVolume(ctx, ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: accountID, CapacityGiB: uint64(size), State: string(ebsprovider.VolumeStateAvailable),
		CreatedAt: now, AvailabilityZone: *input.AvailabilityZone, VolumeType: volumeType,
		IOPS: iops, Throughput: throughput, SnapshotID: snapshotID, Tags: tags, ProviderHandle: created.Handle,
		Encrypted: mkey != nil,
	}); err != nil {
		slog.ErrorContext(ctx, "CreateVolume: failed to persist provider metadata", "volumeId", volumeID, "err", err)
		_ = s.provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, Handle: created.Handle})
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "CreateVolume completed", "volumeId", volumeID, "size", size, "type", volumeType)

	return &ec2.Volume{VolumeId: aws.String(volumeID), Size: aws.Int64(size), VolumeType: aws.String(volumeType),
		State: aws.String("available"), AvailabilityZone: input.AvailabilityZone, CreateTime: aws.Time(now),
		Iops: aws.Int64(int64(iops)), Throughput: aws.Int64(int64(throughput)), Encrypted: aws.Bool(mkey != nil),
		SnapshotId: snapshotIDOrNil(snapshotID), Tags: utils.MapToEC2Tags(tags)}, nil
}

// snapshotIDOrNil keeps SnapshotId absent for a volume that was not cloned,
// rather than serialising an empty string.
func snapshotIDOrNil(snapshotID string) *string {
	if snapshotID == "" {
		return nil
	}
	return aws.String(snapshotID)
}

// requireProvider fails the call when no EBS provider is wired. Every write
// path delegates allocation to the provider, so a nil one is a wiring fault
// rather than a mode the control plane can serve.
func (s *VolumeServiceImpl) requireProvider(ctx context.Context, op string) error {
	if s.provider == nil {
		slog.ErrorContext(ctx, "no EBS provider configured", "op", op)
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
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

// DescribeVolumes lists EBS volumes by reading config.json files from S3.
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

	// Fast path: specific volume IDs requested. Fetch each document
	// directly instead of enumerating every volume in the bucket.
	if len(input.VolumeIds) > 0 {
		results := s.fetchVolumesByIDs(ctx, input.VolumeIds, accountID)
		volumes := make([]*ec2.Volume, 0, len(results))
		for _, r := range results {
			if r.err != nil {
				return nil, r.err
			}
			if r.volume == nil {
				continue // nil VolumeIds entry
			}
			if len(parsedFilters) == 0 || volumeMatchesFilters(r.volume, parsedFilters) {
				volumes = append(volumes, r.volume)
			}
		}
		slog.InfoContext(ctx, "DescribeVolumes completed", "count", len(volumes))
		return &ec2.DescribeVolumesOutput{Volumes: volumes}, nil
	}

	// Slow path: no specific IDs requested, list the caller's prefix. No other
	// account's document is read, so none has to be filtered out here.
	metadata, err := s.metadata.ListVolumes(ctx, accountID)
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	volumes := make([]*ec2.Volume, 0, len(metadata))
	for _, meta := range metadata {
		volume := metadataVolumeToEC2(meta)
		if len(parsedFilters) == 0 || volumeMatchesFilters(volume, parsedFilters) {
			volumes = append(volumes, volume)
		}
	}
	slog.InfoContext(ctx, "DescribeVolumes completed", "count", len(volumes))
	return &ec2.DescribeVolumesOutput{Volumes: volumes}, nil
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

	return s.describeVolumeStatus(ctx, input, accountID, parsedFilters)
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

// describeVolumeStatus answers from the ebsmetadata index. Volume health is
// static: the control plane has no per-volume health signal to report, so
// every known volume reports ok.
func (s *VolumeServiceImpl) describeVolumeStatus(ctx context.Context, input *ec2.DescribeVolumeStatusInput, accountID string, parsedFilters map[string][]string) (*ec2.DescribeVolumeStatusOutput, error) {
	var statusItems []*ec2.VolumeStatusItem

	// Fast path: specific volume IDs requested. Fetch each document directly
	// rather than listing the prefix to answer for a handful of volumes.
	if len(input.VolumeIds) > 0 {
		for _, r := range s.fetchVolumeMetasByIDs(ctx, input.VolumeIds, accountID) {
			if r.err != nil {
				return nil, r.err
			}
			if r.absent {
				continue // nil VolumeIds entry
			}
			item := metadataVolumeStatusItem(r.meta)
			if len(parsedFilters) > 0 && !volumeStatusMatchesFilters(item, parsedFilters) {
				continue
			}
			statusItems = append(statusItems, item)
		}
		slog.InfoContext(ctx, "DescribeVolumeStatus completed", "count", len(statusItems))
		return &ec2.DescribeVolumeStatusOutput{VolumeStatuses: statusItems}, nil
	}

	metas, err := s.metadata.ListVolumes(ctx, accountID)
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	for _, meta := range metas {
		item := metadataVolumeStatusItem(meta)
		if len(parsedFilters) > 0 && !volumeStatusMatchesFilters(item, parsedFilters) {
			continue
		}
		statusItems = append(statusItems, item)
	}

	slog.InfoContext(ctx, "DescribeVolumeStatus completed", "count", len(statusItems))
	return &ec2.DescribeVolumeStatusOutput{VolumeStatuses: statusItems}, nil
}

// metadataVolumeStatusItem builds a static-health VolumeStatusItem from an
// ebsmetadata document.
func metadataVolumeStatusItem(meta ebsmetadata.Volume) *ec2.VolumeStatusItem {
	return &ec2.VolumeStatusItem{
		VolumeId:         aws.String(meta.VolumeID),
		AvailabilityZone: aws.String(meta.AvailabilityZone),
		VolumeStatus: &ec2.VolumeStatusInfo{
			Status: aws.String("ok"),
			Details: []*ec2.VolumeStatusDetails{
				{Name: aws.String("io-enabled"), Status: aws.String("passed")},
				{Name: aws.String("io-performance"), Status: aws.String("not-applicable")},
			},
		},
		Actions: []*ec2.VolumeStatusAction{},
		Events:  []*ec2.VolumeStatusEvent{},
	}
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

// ebsModificationToEC2 converts a persisted ebsmetadata.VolumeModification
// into the AWS SDK shape returned by ModifyVolume /
// DescribeVolumesModifications. volumeID comes from the owning
// ebsmetadata.Volume, since VolumeModification does not duplicate it.
func ebsModificationToEC2(volumeID string, m *ebsmetadata.VolumeModification) *ec2.VolumeModification {
	if m == nil {
		return nil
	}
	out := &ec2.VolumeModification{
		VolumeId:           aws.String(volumeID),
		ModificationState:  aws.String(m.ModificationState),
		Progress:           aws.Int64(m.Progress),
		OriginalSize:       aws.Int64(m.OriginalSize),
		OriginalIops:       aws.Int64(m.OriginalIOPS),
		OriginalVolumeType: aws.String(m.OriginalVolumeType),
		TargetSize:         aws.Int64(m.TargetSize),
		TargetIops:         aws.Int64(m.TargetIOPS),
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

	if len(input.VolumeIds) > 0 {
		results := s.fetchVolumeModificationsByIDs(ctx, input.VolumeIds, accountID)
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

	metas, err := s.metadata.ListVolumes(ctx, accountID)
	if err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	var volumeIDFilterValues []string
	if parsedFilters != nil {
		volumeIDFilterValues = parsedFilters["volume-id"]
	}

	for _, meta := range metas {
		if len(volumeIDFilterValues) > 0 && !filterutil.MatchesAny(volumeIDFilterValues, meta.VolumeID) {
			continue
		}
		if meta.Modification == nil {
			continue
		}
		mod := ebsModificationToEC2(meta.VolumeID, meta.Modification)
		if len(parsedFilters) > 0 && !volumeModificationMatchesFilters(mod, parsedFilters) {
			continue
		}
		modifications = append(modifications, mod)
	}

	slog.InfoContext(ctx, "DescribeVolumesModifications completed", "count", len(modifications))
	return &ec2.DescribeVolumesModificationsOutput{VolumesModifications: modifications}, nil
}

// volumeModificationResult bundles a per-ID lookup result so the fast path
// can preserve input ordering and surface errors after the parallel fan-out.
type volumeModificationResult struct {
	modification *ec2.VolumeModification
	err          error
}

// fetchVolumeModificationsByIDs reads each requested volume's document in
// parallel, returning results positionally aligned with volumeIDs.
// Cross-tenant volumes surface as InvalidVolume.NotFound.
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
			meta, err := s.metadata.GetVolume(ctx, accountID, volID)
			if err != nil || meta.TenantID != accountID {
				results[idx] = volumeModificationResult{err: errors.New(awserrors.ErrorInvalidVolumeNotFound)}
				return
			}
			results[idx] = volumeModificationResult{modification: ebsModificationToEC2(meta.VolumeID, meta.Modification)}
		}(i, *volumeID)
	}

	wg.Wait()
	return results
}

// volumeFetchResult bundles a single volume-ID lookup result so the fast path
// can preserve input ordering and surface per-ID errors after the parallel
// fan-out, matching fetchVolumeModificationsByIDs.
type volumeFetchResult struct {
	volume *ec2.Volume
	err    error
}

// fetchVolumesByIDs fetches each requested volume's ebsmetadata document
// directly via GetVolume, instead of listing the account's prefix.
func (s *VolumeServiceImpl) fetchVolumesByIDs(ctx context.Context, volumeIDs []*string, accountID string) []volumeFetchResult {
	metas := s.fetchVolumeMetasByIDs(ctx, volumeIDs, accountID)
	results := make([]volumeFetchResult, len(metas))
	for i, r := range metas {
		switch {
		case r.err != nil:
			results[i] = volumeFetchResult{err: r.err}
		case r.absent:
		default:
			results[i] = volumeFetchResult{volume: metadataVolumeToEC2(r.meta)}
		}
	}
	return results
}

// volumeMetaResult bundles a per-ID document lookup. absent marks a nil entry
// in the requested IDs, which is neither a document nor an error.
type volumeMetaResult struct {
	meta   ebsmetadata.Volume
	absent bool
	err    error
}

// fetchVolumeMetasByIDs reads each requested volume's document in parallel,
// returning results positionally aligned with volumeIDs. A missing document or
// a cross-tenant volume both surface as InvalidVolume.NotFound; any other store
// error surfaces as ErrorServerInternal.
func (s *VolumeServiceImpl) fetchVolumeMetasByIDs(ctx context.Context, volumeIDs []*string, accountID string) []volumeMetaResult {
	results := make([]volumeMetaResult, len(volumeIDs))
	var wg sync.WaitGroup

	for i, id := range volumeIDs {
		if id == nil {
			results[i] = volumeMetaResult{absent: true}
			continue
		}
		wg.Add(1)
		go func(idx int, volID string) {
			defer wg.Done()
			meta, err := s.metadata.GetVolume(ctx, accountID, volID)
			if err != nil {
				if objectstore.IsNoSuchKeyError(err) {
					results[idx] = volumeMetaResult{err: errors.New(awserrors.ErrorInvalidVolumeNotFound)}
				} else {
					results[idx] = volumeMetaResult{err: errors.New(awserrors.ErrorServerInternal)}
				}
				return
			}
			// The read was scoped to the caller's prefix, so the document is
			// theirs by construction. The comparison stays as an assertion.
			if meta.TenantID != accountID {
				results[idx] = volumeMetaResult{err: errors.New(awserrors.ErrorInvalidVolumeNotFound)}
				return
			}
			results[idx] = volumeMetaResult{meta: meta}
		}(i, *id)
	}

	wg.Wait()
	return results
}

// metadataVolumeToEC2 renders an ebsmetadata document as the AWS SDK volume
// shape. Fields that are zero because they were never set are omitted rather
// than surfaced as a misleading 0 or empty string.
func metadataVolumeToEC2(meta ebsmetadata.Volume) *ec2.Volume {
	// An empty State is internal drift, not a valid AWS state. Derive the
	// effective state from ground truth (the attachment) rather than blindly
	// rendering "available", which would hide an empty-but-attached volume.
	state := meta.State
	if state == "" {
		if meta.AttachedInstance != "" {
			state = "in-use"
		} else {
			state = "available"
		}
	}
	volumeType := meta.VolumeType
	if volumeType == "" {
		volumeType = "gp3"
	}
	volume := &ec2.Volume{
		VolumeId: aws.String(meta.VolumeID), Size: aws.Int64(safecast.Uint64ToInt64(meta.CapacityGiB)),
		State: aws.String(state), AvailabilityZone: aws.String(meta.AvailabilityZone), CreateTime: aws.Time(meta.CreatedAt),
		VolumeType: aws.String(volumeType), Encrypted: aws.Bool(meta.Encrypted), Tags: utils.MapToEC2Tags(meta.Tags),
	}
	if meta.IOPS > 0 {
		volume.Iops = aws.Int64(int64(meta.IOPS))
	}
	if meta.Throughput > 0 {
		volume.Throughput = aws.Int64(int64(meta.Throughput))
	}
	if meta.SnapshotID != "" {
		volume.SnapshotId = aws.String(meta.SnapshotID)
	}
	if meta.AttachedInstance != "" {
		// A recorded attachment on a volume that is not in-use is drift, so the
		// attachment reports detached rather than claiming a live attach.
		attachState := "attached"
		if meta.State != "in-use" {
			attachState = "detached"
		}
		volume.Attachments = []*ec2.VolumeAttachment{{VolumeId: aws.String(meta.VolumeID), InstanceId: aws.String(meta.AttachedInstance), Device: aws.String(meta.DeviceName), State: aws.String(attachState), DeleteOnTermination: aws.Bool(meta.DeleteOnTermination), AttachTime: aws.Time(meta.AttachedAt)}}
	}
	return volume
}

// GetVolumeMetadata reads the control-plane document for a volume. It is the
// ownership and attachment record the daemon validates attach/detach against.
func (s *VolumeServiceImpl) GetVolumeMetadata(accountID, volumeID string) (ebsmetadata.Volume, error) {
	return s.getVolumeMetadata(context.Background(), accountID, volumeID)
}

// getVolumeMetadata is GetVolumeMetadata carrying the caller's context. A
// missing document is remapped to InvalidVolume.NotFound so callers surface the
// AWS error rather than a store error.
func (s *VolumeServiceImpl) getVolumeMetadata(ctx context.Context, accountID, volumeID string) (ebsmetadata.Volume, error) {
	meta, err := s.metadata.GetVolume(ctx, accountID, volumeID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			slog.WarnContext(ctx, "volume metadata not found", "volumeId", volumeID)
			return ebsmetadata.Volume{}, errors.New(awserrors.ErrorInvalidVolumeNotFound)
		}
		return ebsmetadata.Volume{}, fmt.Errorf("get volume metadata: %w", err)
	}
	return meta, nil
}

// UpdateVolumeState updates the control-plane-owned attachment state (state,
// attachment, device) on the volume's ebsmetadata document.
func (s *VolumeServiceImpl) UpdateVolumeState(accountID, volumeID, state, attachedInstance, deviceName string) error {
	ctx := context.Background()
	meta, err := s.metadata.GetVolume(ctx, accountID, volumeID)
	if err != nil {
		return fmt.Errorf("failed to get volume metadata: %w", err)
	}

	// A detached volume is "available": never persist an empty State for an
	// unattached volume, so a detach/terminate writeback that omits the state
	// cannot strand the volume in drift that later reads as undeletable.
	if state == "" && attachedInstance == "" {
		state = "available"
	}
	meta.State = state
	meta.AttachedInstance = attachedInstance
	meta.DeviceName = deviceName
	if attachedInstance != "" {
		meta.AttachedAt = time.Now()
	} else {
		meta.AttachedAt = time.Time{}
	}
	if err := s.metadata.PutVolume(ctx, meta); err != nil {
		return fmt.Errorf("failed to write volume metadata: %w", err)
	}

	slog.Info("Updated volume state", "volumeId", volumeID, "state", state, "attachedInstance", attachedInstance, "deviceName", deviceName)
	return nil
}

// ModifyVolume modifies an EBS volume (grow-only, requires stopped instance).
func (s *VolumeServiceImpl) ModifyVolume(ctx context.Context, input *ec2.ModifyVolumeInput, accountID string) (*ec2.ModifyVolumeOutput, error) {
	if input == nil || input.VolumeId == nil || *input.VolumeId == "" {
		return nil, errors.New(awserrors.ErrorInvalidVolumeIDMalformed)
	}

	volumeID := *input.VolumeId
	slog.InfoContext(ctx, "ModifyVolume request", "volumeId", volumeID)

	if err := s.requireProvider(ctx, "ModifyVolume"); err != nil {
		return nil, err
	}

	meta, err := s.metadata.GetVolume(ctx, accountID, volumeID)
	if err != nil || meta.TenantID != accountID {
		return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}
	originalSize := safecast.Uint64ToInt64(meta.CapacityGiB)
	if input.Size == nil || *input.Size <= originalSize {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if meta.AttachedInstance != "" && meta.State == "in-use" {
		return nil, errors.New(awserrors.ErrorIncorrectState)
	}
	originalType := meta.VolumeType
	if originalType == "" {
		originalType = "gp3"
	}
	originalIOPS := int64(meta.IOPS)

	expanded, err := s.provider.ExpandVolume(ctx, ebsprovider.ExpandVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, Handle: meta.ProviderHandle,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: *input.Size * 1024 * 1024 * 1024},
	})
	if err != nil || expanded == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	meta.CapacityGiB = safecast.Int64ToUint64(*input.Size)
	if input.VolumeType != nil {
		meta.VolumeType = *input.VolumeType
	}
	if input.Iops != nil {
		meta.IOPS = int(*input.Iops)
	}
	targetType := meta.VolumeType
	if targetType == "" {
		targetType = "gp3"
	}

	// Persist the modification record on the volume document so a subsequent
	// DescribeVolumesModifications can read it back. Modifications are
	// synchronous, so state is always completed/100.
	now := time.Now()
	meta.Modification = &ebsmetadata.VolumeModification{
		ModificationState:  "completed",
		Progress:           100,
		OriginalSize:       originalSize,
		OriginalIOPS:       originalIOPS,
		OriginalVolumeType: originalType,
		TargetSize:         safecast.Uint64ToInt64(meta.CapacityGiB),
		TargetIOPS:         int64(meta.IOPS),
		TargetVolumeType:   targetType,
		StartTime:          now,
		EndTime:            now,
	}
	if err := s.metadata.PutVolume(ctx, meta); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "ModifyVolume completed", "volumeId", volumeID,
		"originalSize", originalSize, "targetSize", meta.Modification.TargetSize)

	return &ec2.ModifyVolumeOutput{VolumeModification: ebsModificationToEC2(volumeID, meta.Modification)}, nil
}

// DeleteVolumeOnTerminate deletes a DeleteOnTermination volume as part of an
// instance terminate: terminate implies detach, so this clears any stale
// attachment via UpdateVolumeState before calling DeleteVolume. Both the
// stopped-instance path (Stop's Unmount deliberately never clears a Boot
// volume, vm_adapters.go's volumeMounterAdapter.Unmount) and the
// running-instance path (terminateCleanup runs after shutdownAndUnmount,
// which has the same Boot-volume carve-out) would otherwise still have
// AttachedInstance set and hit DeleteVolume's in-use guard. There is no live
// QEMU to hot-unplug on either path — a stopped instance has none, and a
// terminating instance's QEMU has already been asked to shut down — so this
// is always a metadata-only clear, never a QMP call. Errors from either step
// are returned, not swallowed.
func (s *VolumeServiceImpl) DeleteVolumeOnTerminate(ctx context.Context, volumeID, accountID string) error {
	if err := s.UpdateVolumeState(accountID, volumeID, "available", "", ""); err != nil {
		// No metadata doc means the volume is already gone, per GetVolume's own
		// contract, so absence here is success, not a teardown failure.
		if objectstore.IsNoSuchKeyError(err) {
			return nil
		}
		return fmt.Errorf("clear attachment before terminate delete: %w", err)
	}
	_, err := s.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: &volumeID}, accountID)
	return err
}

// DetachVolumeOnTerminate clears a volume's attachment on instance terminate
// without deleting it, matching AWS semantics for a DeleteOnTermination=false
// volume: terminate still implies detach, it just leaves the volume behind as
// available rather than deleting it. Metadata-only, no QMP — same rationale
// as DeleteVolumeOnTerminate: there is no live QEMU to hot-unplug on either
// terminate path.
func (s *VolumeServiceImpl) DetachVolumeOnTerminate(_ context.Context, volumeID, accountID string) error {
	if err := s.UpdateVolumeState(accountID, volumeID, "available", "", ""); err != nil {
		// No metadata doc means the volume is already gone, per GetVolume's own
		// contract, so absence here is success, not a teardown failure.
		if objectstore.IsNoSuchKeyError(err) {
			return nil
		}
		return err
	}
	return nil
}

// ForceDetachVolume clears a volume's attachment in the control plane without
// touching the guest, and is answered by any node rather than the one hosting
// the instance.
//
// The ordinary DetachVolume routes to ec2.cmd.{instanceID} for the QMP unplug,
// so a volume attached to an instance whose host stopped answering can never be
// detached and therefore never deleted. This exists for that deadlock and for
// nothing else: it leaves a live guest holding a device the control plane no
// longer believes in, which is only safe once that guest is being destroyed.
func (s *VolumeServiceImpl) ForceDetachVolume(ctx context.Context, input *ec2.DetachVolumeInput, accountID string) (*ec2.VolumeAttachment, error) {
	if input == nil || input.VolumeId == nil || *input.VolumeId == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	volumeID := *input.VolumeId

	meta, err := s.metadata.GetVolume(ctx, accountID, volumeID)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}
	if meta.TenantID != accountID {
		return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}

	previous := meta.AttachedInstance
	if err := s.UpdateVolumeState(accountID, volumeID, "available", "", ""); err != nil {
		slog.ErrorContext(ctx, "ForceDetachVolume: failed to clear attachment", "volumeId", volumeID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	slog.WarnContext(ctx, "ForceDetachVolume: attachment cleared in the control plane only",
		"volumeId", volumeID, "previousInstance", previous, "accountId", accountID)

	return &ec2.VolumeAttachment{
		VolumeId:   aws.String(volumeID),
		InstanceId: aws.String(previous),
		State:      aws.String("detached"),
	}, nil
}

// DeleteVolume deletes an EBS volume: validates state, asks the EBS provider to
// destroy the backing data, then removes the control-plane metadata document.
func (s *VolumeServiceImpl) DeleteVolume(ctx context.Context, input *ec2.DeleteVolumeInput, accountID string) (*ec2.DeleteVolumeOutput, error) {
	if input == nil || input.VolumeId == nil || *input.VolumeId == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	volumeID := *input.VolumeId
	slog.InfoContext(ctx, "DeleteVolume request", "volumeId", volumeID)

	if err := s.requireProvider(ctx, "DeleteVolume"); err != nil {
		return nil, err
	}

	// AWS-faithful: an absent volume returns InvalidVolume.NotFound.
	meta, err := s.metadata.GetVolume(ctx, accountID, volumeID)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}
	if meta.TenantID != accountID {
		return nil, errors.New(awserrors.ErrorInvalidVolumeNotFound)
	}

	// An unattached volume is deletable. State must be "available" OR empty: a
	// detach/terminate that failed to write back "available" leaves the State
	// drifted to empty with no attachment, and gating on State=="available"
	// exactly would strand it undeletable and block stack teardown.
	if meta.AttachedInstance != "" || (meta.State != "available" && meta.State != "") {
		slog.ErrorContext(ctx, "DeleteVolume: volume is in use", "volumeId", volumeID, "state", meta.State, "attachedInstance", meta.AttachedInstance)
		return nil, errors.New(awserrors.ErrorVolumeInUse)
	}

	// Snapshot-backed clones read chunk files from the source volume's S3
	// prefix, so deleting a volume that still has snapshots would break them.
	if err := s.checkVolumeHasNoSnapshots(ctx, volumeID); err != nil {
		return nil, err
	}
	if err := s.provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, Handle: meta.ProviderHandle,
	}); err != nil {
		slog.ErrorContext(ctx, "DeleteVolume: provider deletion failed", "volumeId", volumeID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if err := s.metadata.DeleteVolume(ctx, accountID, volumeID); err != nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "DeleteVolume completed", "volumeId", volumeID)

	return &ec2.DeleteVolumeOutput{}, nil
}

// checkVolumeHasNoSnapshots checks if a volume has dependent snapshots
// using the JetStream KV index.
func (s *VolumeServiceImpl) checkVolumeHasNoSnapshots(ctx context.Context, volumeID string) error {
	if s.snapshotKV == nil {
		slog.ErrorContext(ctx, "checkVolumeHasNoSnapshots: snapshotKV is nil", "volumeId", volumeID)
		return errors.New(awserrors.ErrorServerInternal)
	}

	has, err := s.volumeHasSnapshotsKV(ctx, volumeID)
	if err != nil {
		slog.ErrorContext(ctx, "checkVolumeHasNoSnapshots: KV lookup failed", "volumeId", volumeID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if has {
		slog.ErrorContext(ctx, "DeleteVolume blocked: volume has snapshots", "volumeId", volumeID)
		return errors.New(awserrors.ErrorVolumeInUse)
	}
	return nil
}

// volumeHasSnapshotsKV checks the JetStream KV index for snapshot references.
func (s *VolumeServiceImpl) volumeHasSnapshotsKV(ctx context.Context, volumeID string) (bool, error) {
	entry, err := s.snapshotKV.Get(ctx, volumeID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
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

// ApplyRecordTags mirrors CreateTags into the owning volume's tags.json so
// DescribeVolumes observes tags added after create. Non-vol ids, volumes absent
// from this store, and volumes the caller does not own are skipped.
func (s *VolumeServiceImpl) ApplyRecordTags(input *ec2.CreateTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return s.mirrorVolumeTags(context.Background(), input.Resources, accountID, utils.MergeTagsMut(input))
}

// RemoveRecordTags mirrors DeleteTags into the owning volume's tags.json with
// AWS-faithful delete semantics.
func (s *VolumeServiceImpl) RemoveRecordTags(input *ec2.DeleteTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	return s.mirrorVolumeTags(context.Background(), input.Resources, accountID, utils.RemoveTagsMut(input))
}

// mirrorVolumeTags read-modify-writes the ebsmetadata document for each vol-
// id. The document supplies the ownership gate. Mismatch or absence is a no-op.
func (s *VolumeServiceImpl) mirrorVolumeTags(ctx context.Context, resources []*string, accountID string, mut func(map[string]string)) error {
	for _, res := range resources {
		if res == nil || !strings.HasPrefix(*res, "vol-") {
			continue
		}
		if err := s.mirrorVolumeTagsOne(ctx, *res, accountID, mut); err != nil {
			return err
		}
	}
	return nil
}

// mirrorVolumeTagsOne read-modify-writes one ebsmetadata.Volume's Tags field.
func (s *VolumeServiceImpl) mirrorVolumeTagsOne(ctx context.Context, volumeID, accountID string, mut func(map[string]string)) error {
	meta, err := s.metadata.GetVolume(ctx, accountID, volumeID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil
		}
		return err
	}
	if meta.TenantID != accountID {
		slog.Debug("mirrorVolumeTags: skipping volume not owned by caller", "volumeId", volumeID)
		return nil
	}

	tags := meta.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	mut(tags)
	meta.Tags = tags
	return s.metadata.PutVolume(ctx, meta)
}
