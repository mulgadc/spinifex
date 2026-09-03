package handlers_ec2_image

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"
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
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Ensure ImageServiceImpl implements ImageService.
var _ ImageService = (*ImageServiceImpl)(nil)

// CreateImageParams holds parameters for creating an AMI from an instance.
// Used by the daemon handler which extracts instance state before calling the service.
type CreateImageParams struct {
	Input         *ec2.CreateImageInput
	RootVolumeID  string
	SourceImageID string
	IsRunning     bool // true = use live checkpoint (instance still running), false = use numbered checkpoint from Close
}

// ImageServiceImpl handles AMI image operations with S3 storage.
type ImageServiceImpl struct {
	config     *config.Config
	store      objectstore.ObjectStore
	bucketName string
	natsConn   *nats.Conn
	metadata   *ebsmetadata.Store
	provider   ebsprovider.EBSProvider
}

// SetEBSProvider injects the provider boundary used by the control plane.
// Legacy AMI/snapshot paths remain in place until their metadata migration is complete.
func (s *ImageServiceImpl) SetEBSProvider(provider ebsprovider.EBSProvider) {
	s.provider = provider
}

// EBSProvider returns the injected provider boundary, or nil on the legacy
// embedded-engine path. Primarily for composition-root tests to observe wiring.
func (s *ImageServiceImpl) EBSProvider() ebsprovider.EBSProvider {
	return s.provider
}

// MetadataStore returns the control-plane metadata store. Primarily for
// composition-root tests to observe wiring.
func (s *ImageServiceImpl) MetadataStore() *ebsmetadata.Store {
	return s.metadata
}

// NewImageServiceImpl creates a new daemon-side image service. natsConn is used
// to drain a running instance's volume (routed to the node hosting it) before
// CreateImageFromInstance reads its live checkpoint.
func NewImageServiceImpl(cfg *config.Config, natsConn *nats.Conn) *ImageServiceImpl {
	store := objectstore.NewS3ObjectStoreFromConfig(
		cfg.Predastore.Host,
		cfg.Predastore.Region,
		cfg.Predastore.AccessKey,
		cfg.Predastore.SecretKey,
	)

	return &ImageServiceImpl{
		config:     cfg,
		store:      store,
		bucketName: cfg.Predastore.Bucket,
		natsConn:   natsConn,
		metadata:   ebsmetadata.NewStore(store, cfg.Predastore.Bucket),
	}
}

// NewImageServiceImplWithStore creates an image service with a custom object store (for testing).
func NewImageServiceImplWithStore(store objectstore.ObjectStore, bucketName string) *ImageServiceImpl {
	return &ImageServiceImpl{
		store:      store,
		bucketName: bucketName,
		metadata:   ebsmetadata.NewStore(store, bucketName),
	}
}

// NewImageServiceImplWithConfig creates an image service with an explicit
// config and NATS connection (for testing the running-volume drain path,
// which needs config.DataDir/Predastore and a NATS connection to route to).
func NewImageServiceImplWithConfig(cfg *config.Config, store objectstore.ObjectStore, natsConn *nats.Conn) *ImageServiceImpl {
	return &ImageServiceImpl{
		config:     cfg,
		store:      store,
		bucketName: cfg.Predastore.Bucket,
		natsConn:   natsConn,
		metadata:   ebsmetadata.NewStore(store, cfg.Predastore.Bucket),
	}
}

// describeImagesValidFilters defines the set of filter names accepted by DescribeImages.
var describeImagesValidFilters = map[string]bool{
	"name":                true,
	"state":               true,
	"architecture":        true,
	"image-id":            true,
	"is-public":           true,
	"owner-id":            true,
	"description":         true,
	"image-type":          true,
	"virtualization-type": true,
	"root-device-type":    true,
}

// DescribeImages lists available AMI images by reading config.json files from S3.
func (s *ImageServiceImpl) DescribeImages(ctx context.Context, input *ec2.DescribeImagesInput, accountID string) (*ec2.DescribeImagesOutput, error) {
	if input == nil {
		input = &ec2.DescribeImagesInput{}
	}

	slog.InfoContext(ctx, "Describing images", "filters", input.Filters, "imageIds", input.ImageIds)

	parsedFilters, err := filterutil.ParseFilters(input.Filters, describeImagesValidFilters)
	if err != nil {
		slog.WarnContext(ctx, "DescribeImages: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	return s.describeImages(ctx, input, accountID, parsedFilters)
}

// describeImages enumerates AMIs via Store.ListAMIs, then filters and renders
// each document into the AWS SDK image shape.
func (s *ImageServiceImpl) describeImages(ctx context.Context, input *ec2.DescribeImagesInput, accountID string, parsedFilters map[string][]string) (*ec2.DescribeImagesOutput, error) {
	amis, err := s.metadata.ListAMIs(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "DescribeImages: failed to list AMIs", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	encryptedAtRest := s.clusterEncryptionEnabled()
	var images []*ec2.Image

	for _, amiMeta := range amis {
		if amiMeta.ImageID == "" {
			continue
		}

		if len(input.ImageIds) > 0 {
			found := false
			for _, filterID := range input.ImageIds {
				if filterID != nil && *filterID == amiMeta.ImageID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		amiOwner := amiMeta.ImageOwnerAlias
		isSystemAMI := amiOwner != "" && !utils.IsAccountID(amiOwner)

		if !callerCanReadAMI(amiMeta, accountID) {
			continue
		}

		if len(input.Owners) > 0 {
			found := false
			for _, owner := range input.Owners {
				if owner == nil {
					continue
				}
				switch *owner {
				case "self":
					if amiOwner == accountID {
						found = true
					}
				default:
					if amiOwner == *owner {
						found = true
					} else if isSystemAMI && *owner == utils.GlobalAccountID {
						found = true
					}
				}
				if found {
					break
				}
			}
			if !found {
				continue
			}
		}

		ownerID := amiOwner
		if isSystemAMI {
			ownerID = utils.GlobalAccountID
		}

		image := &ec2.Image{
			ImageId:            aws.String(amiMeta.ImageID),
			Name:               aws.String(amiMeta.Name),
			Description:        aws.String(amiMeta.Description),
			Architecture:       aws.String(amiMeta.Architecture),
			PlatformDetails:    aws.String(amiMeta.PlatformDetails),
			Platform:           utils.PlatformFromDetails(amiMeta.PlatformDetails),
			CreationDate:       aws.String(amiMeta.CreationDate.Format("2006-01-02T15:04:05.000Z")),
			RootDeviceType:     aws.String(amiMeta.RootDeviceType),
			VirtualizationType: aws.String(amiMeta.Virtualization),
			ImageOwnerAlias:    aws.String(amiMeta.ImageOwnerAlias),
			OwnerId:            aws.String(ownerID),
			Public:             aws.Bool(false),
			State:              aws.String(amiImageState(amiMeta.State)),
			ImageType:          aws.String("machine"),
			Hypervisor:         aws.String("xen"),
		}

		// RegisterImage takes BootMode as an optional input, so an AMI can
		// record none. The enum has no empty member: omit it, don't emit "".
		if amiMeta.BootMode != "" {
			image.BootMode = aws.String(amiMeta.BootMode)
		}

		if bdms := synthesizeRootBlockDeviceMapping(amiMeta, encryptedAtRest); bdms != nil {
			image.RootDeviceName = aws.String("/dev/sda1")
			image.BlockDeviceMappings = bdms
		}

		image.Tags = utils.MapToEC2Tags(amiMeta.Tags)

		if len(parsedFilters) > 0 && !imageMatchesFilters(image, parsedFilters, amiMeta.Tags) {
			continue
		}

		images = append(images, image)
	}

	if len(input.ImageIds) > 0 {
		foundIDs := make(map[string]bool, len(images))
		for _, img := range images {
			if img.ImageId != nil {
				foundIDs[*img.ImageId] = true
			}
		}
		for _, reqID := range input.ImageIds {
			if reqID != nil && !foundIDs[*reqID] {
				return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
			}
		}
	}

	slog.InfoContext(ctx, "DescribeImages completed", "count", len(images))
	return &ec2.DescribeImagesOutput{Images: images}, nil
}

// amiImageState maps AMIMetadata.State to the ec2.Image state string. An
// empty State means the AMI was registered before the field existed, and
// MUST report as "available" — those images are already complete and
// launchable, so treating empty as anything else would hide them.
func amiImageState(state string) string {
	if state == "" {
		return "available"
	}
	return state
}

// imageMatchesFilters checks whether an ec2.Image satisfies all parsed filters.
func imageMatchesFilters(image *ec2.Image, filters map[string][]string, tags map[string]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}

		var field string
		switch name {
		case "name":
			if image.Name != nil {
				field = *image.Name
			}
		case "state":
			if image.State != nil {
				field = *image.State
			}
		case "architecture":
			if image.Architecture != nil {
				field = *image.Architecture
			}
		case "image-id":
			if image.ImageId != nil {
				field = *image.ImageId
			}
		case "is-public":
			if image.Public != nil {
				field = strconv.FormatBool(*image.Public)
			}
		case "owner-id":
			if image.OwnerId != nil {
				field = *image.OwnerId
			}
		case "description":
			if image.Description != nil {
				field = *image.Description
			}
		case "image-type":
			if image.ImageType != nil {
				field = *image.ImageType
			}
		case "virtualization-type":
			if image.VirtualizationType != nil {
				field = *image.VirtualizationType
			}
		case "root-device-type":
			if image.RootDeviceType != nil {
				field = *image.RootDeviceType
			}
		default:
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	return filterutil.MatchesTags(filters, tags)
}

// CreateImage is the generic interface method — on the daemon side, the handler
// calls CreateImageFromInstance directly with the extra instance context.
func (s *ImageServiceImpl) CreateImage(ctx context.Context, input *ec2.CreateImageInput, accountID string) (*ec2.CreateImageOutput, error) {
	return nil, errors.New("CreateImage requires instance context; use CreateImageFromInstance on daemon side")
}

// CreateImageFromInstance creates an AMI from an instance by snapshotting the root
// volume and storing a new AMI config in S3.
func (s *ImageServiceImpl) CreateImageFromInstance(params CreateImageParams, accountID string) (*ec2.CreateImageOutput, error) {
	input := params.Input
	if input == nil || input.InstanceId == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// Check for duplicate AMI name before doing any expensive work
	name := aws.StringValue(input.Name)
	if name != "" {
		if exists, err := s.amiNameExists(context.Background(), name); err != nil {
			slog.Error("CreateImageFromInstance: failed to check AMI name uniqueness", "name", name, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		} else if exists {
			return nil, errors.New(awserrors.ErrorInvalidAMINameDuplicate)
		}
	}

	amiID := utils.GenerateResourceID("ami")
	snapshotID := utils.GenerateResourceID("snap")

	slog.Info("CreateImageFromInstance", "instanceId", *input.InstanceId,
		"rootVolumeId", params.RootVolumeID, "amiId", amiID, "snapshotId", snapshotID,
		"isRunning", params.IsRunning)

	// Step 1: Snapshot root volume (live via NATS or offline from S3)
	var snapshotErr error
	if params.IsRunning {
		snapshotErr = s.snapshotRunningVolume(params.RootVolumeID, snapshotID, accountID)
	} else {
		snapshotErr = s.snapshotStoppedVolume(params.RootVolumeID, snapshotID, accountID)
	}
	if snapshotErr != nil {
		return nil, snapshotErr
	}

	// Step 2: Read source volume config for size
	volMeta, err := s.getVolumeMetadata(context.Background(), accountID, params.RootVolumeID)
	if err != nil {
		slog.Error("CreateImageFromInstance: failed to read volume metadata", "volumeId", params.RootVolumeID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	volumeSizeGiB := volMeta.CapacityGiB

	// Step 3: Read source AMI config for architecture, platform, etc.
	sourceAMI := ebsmetadata.AMI{
		Architecture:    "x86_64",
		PlatformDetails: "Linux/UNIX",
		Virtualization:  "hvm",
	}
	if params.SourceImageID != "" {
		srcCfg, err := s.GetAMIConfig(context.Background(), params.SourceImageID)
		if err != nil {
			slog.Error("CreateImageFromInstance: failed to read source AMI config", "sourceImageId", params.SourceImageID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		sourceAMI = srcCfg
	}

	// Step 4: Store snapshot metadata
	if err := s.putSnapshotMetadata(context.Background(), snapshotID, params.RootVolumeID, volumeSizeGiB, accountID); err != nil {
		slog.Error("CreateImageFromInstance: failed to write snapshot metadata", "snapshotId", snapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Step 5: Build and store AMI config
	meta := ebsmetadata.AMI{
		ImageID:         amiID,
		Name:            name,
		Description:     aws.StringValue(input.Description),
		SnapshotID:      snapshotID,
		Architecture:    sourceAMI.Architecture,
		PlatformDetails: sourceAMI.PlatformDetails,
		Virtualization:  sourceAMI.Virtualization,
		VolumeSizeGiB:   volumeSizeGiB,
		CreationDate:    time.Now(),
		RootDeviceType:  ec2.DeviceTypeEbs,
		ImageOwnerAlias: accountID,
		BootMode:        sourceAMI.BootMode,
		Distro:          sourceAMI.Distro,
		DistroFamily:    sourceAMI.DistroFamily,
		// Snapshot succeeded before this point, so the image is complete.
		State: "available",
	}

	if err := s.putAMIConfig(context.Background(), amiID, meta); err != nil {
		slog.Error("CreateImageFromInstance: failed to store AMI config", "amiId", amiID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("CreateImageFromInstance completed", "amiId", amiID, "snapshotId", snapshotID)

	return &ec2.CreateImageOutput{
		ImageId: aws.String(amiID),
	}, nil
}

// snapshotRunningVolume creates a snapshot of a running instance's volume by
// draining it to the node hosting it (so any writes buffered there reach S3),
// then reading the live checkpoint written by nbdkit on every NBD Flush. This
// shares handlers/ec2/snapshot's DrainVolume with ec2.CreateSnapshot so the two
// live-snapshot paths cannot diverge on when a drain is required.
func (s *ImageServiceImpl) snapshotRunningVolume(volumeID, snapshotID, accountID string) error {
	if s.config == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	volMeta, err := s.getVolumeMetadata(context.Background(), accountID, volumeID)
	if err != nil {
		slog.Error("snapshotRunningVolume: failed to read volume metadata", "volumeId", volumeID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Flush the writes still buffered by whichever node serves the volume, so
	// the snapshot below is current rather than an arbitrarily stale one
	// predating e.g. a guest-triggered GPT rewrite (growpart on first boot). An
	// attached volume that cannot be drained fails here rather than silently
	// producing an image from a stale checkpoint.
	if err := handlers_ec2_snapshot.DrainVolume(context.Background(), s.config, s.store, s.natsConn, volumeID, volMeta.State, volMeta.AttachedInstance, accountID); err != nil {
		slog.Error("snapshotRunningVolume: drain failed", "volumeId", volumeID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	return s.createSnapshot(context.Background(), volMeta, snapshotID, "snapshotRunningVolume")
}

// snapshotStoppedVolume creates a snapshot of a detached volume. No drain: the
// volume is stopped/detached, so nothing is buffered on a serving node.
// viperblockd's handleCreateSnapshot prefers a live mounted engine over opening
// a second one, so it handles the mounted case itself.
func (s *ImageServiceImpl) snapshotStoppedVolume(volumeID, snapshotID, accountID string) error {
	if s.config == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}

	volMeta, err := s.getVolumeMetadata(context.Background(), accountID, volumeID)
	if err != nil {
		slog.Error("snapshotStoppedVolume: failed to read volume metadata", "volumeId", volumeID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	return s.createSnapshot(context.Background(), volMeta, snapshotID, "snapshotStoppedVolume")
}

// createSnapshot delegates snapshot creation for volMeta to the EBS provider.
// op names the calling path so a failure is attributable in the log.
func (s *ImageServiceImpl) createSnapshot(ctx context.Context, volMeta ebsmetadata.Volume, snapshotID, op string) error {
	if s.provider == nil {
		slog.ErrorContext(ctx, "no EBS provider configured", "op", op)
		return errors.New(awserrors.ErrorServerInternal)
	}
	created, err := s.provider.CreateSnapshot(ctx, ebsprovider.CreateSnapshotRequest{
		Versioned: ebsprovider.NewVersioned(), SnapshotID: snapshotID, VolumeID: volMeta.VolumeID, VolumeHandle: volMeta.ProviderHandle,
	})
	if err != nil || created == nil {
		slog.ErrorContext(ctx, "provider CreateSnapshot failed", "op", op, "volumeId", volMeta.VolumeID, "snapshotId", snapshotID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	slog.InfoContext(ctx, "snapshot created", "op", op, "volumeId", volMeta.VolumeID, "snapshotId", snapshotID)
	return nil
}

// getVolumeMetadata reads a volume's control-plane document, remapping a
// missing document to InvalidVolume.NotFound.
func (s *ImageServiceImpl) getVolumeMetadata(ctx context.Context, accountID, volumeID string) (ebsmetadata.Volume, error) {
	meta, err := s.metadata.GetVolume(ctx, accountID, volumeID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return ebsmetadata.Volume{}, errors.New(awserrors.ErrorInvalidVolumeNotFound)
		}
		return ebsmetadata.Volume{}, fmt.Errorf("get volume metadata: %w", err)
	}
	return meta, nil
}

// amiNameExists checks if any existing AMI already uses the given name. It
// lists strictly: an AMI this cannot read is a name it cannot rule out, and
// answering false would let a duplicate through and mask the corruption.
func (s *ImageServiceImpl) amiNameExists(ctx context.Context, name string) (bool, error) {
	amis, err := s.metadata.ListAMIsStrict(ctx)
	if err != nil {
		return false, fmt.Errorf("amiNameExists: failed to list AMIs: %w", err)
	}
	for _, ami := range amis {
		if ami.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// GetAMIConfig reads an AMI's control-plane document. Errors come from the
// metadata store.
func (s *ImageServiceImpl) GetAMIConfig(ctx context.Context, imageID string) (ebsmetadata.AMI, error) {
	return s.metadata.GetAMI(ctx, imageID)
}

// snapshotAccountForAMI derives the account an AMI's snapshot document is keyed
// under. An AMI's owner field is ImageOwnerAlias, which is an account ID for a
// tenant image and a free-form alias for a system one. derived reports which,
// so the bundled-AMI fallback below cannot drift from this predicate.
func snapshotAccountForAMI(meta ebsmetadata.AMI) (accountID string, derived bool) {
	if utils.IsAccountID(meta.ImageOwnerAlias) {
		return meta.ImageOwnerAlias, false
	}
	return utils.GlobalAccountID, true
}

// GetAMISourceVolumeID returns the volume whose blocks imageID's snapshot
// references, read from the snapshot's document. Bundled system AMIs have no
// standalone snapshot metadata; their snapshot is named after the AMI.
func (s *ImageServiceImpl) GetAMISourceVolumeID(ctx context.Context, imageID string) (string, error) {
	meta, err := s.GetAMIConfig(ctx, imageID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) || errors.Is(err, ebsmetadata.ErrCorruptDocument) {
			return "", errors.New(awserrors.ErrorInvalidAMIIDNotFound)
		}
		slog.ErrorContext(ctx, "GetAMISourceVolumeID: failed to read AMI config", "imageId", imageID, "err", err)
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	if meta.SnapshotID == "" {
		return "", errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	}

	// The snapshot is keyed under its own owner, which for a system AMI is the
	// global account rather than the alias and rather than the caller. Falling
	// back on the caller's miss would silently return an image ID where a
	// volume ID belongs for an imported system AMI.
	snapAccount, derived := snapshotAccountForAMI(meta)
	snapCfg, err := s.metadata.GetSnapshot(ctx, snapAccount, meta.SnapshotID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) && derived {
			slog.WarnContext(ctx, "GetAMISourceVolumeID: system AMI has no snapshot metadata document, falling back to imageID as volume ID",
				"imageId", imageID, "snapshotId", meta.SnapshotID, "imageOwnerAlias", meta.ImageOwnerAlias)
			return imageID, nil
		}
		if objectstore.IsNoSuchKeyError(err) || errors.Is(err, ebsmetadata.ErrCorruptDocument) {
			return "", errors.New(awserrors.ErrorInvalidSnapshotNotFound)
		}
		slog.ErrorContext(ctx, "GetAMISourceVolumeID: failed to read snapshot metadata",
			"imageId", imageID, "snapshotId", meta.SnapshotID, "err", err)
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	if snapCfg.VolumeID == "" {
		slog.ErrorContext(ctx, "GetAMISourceVolumeID: snapshot metadata has no source volume",
			"imageId", imageID, "snapshotId", meta.SnapshotID)
		return "", errors.New(awserrors.ErrorInvalidSnapshotNotFound)
	}
	return snapCfg.VolumeID, nil
}

// putAMIConfig writes AMI metadata to the ebsmetadata document store.
func (s *ImageServiceImpl) putAMIConfig(ctx context.Context, _ string, meta ebsmetadata.AMI) error {
	return s.metadata.PutAMI(ctx, meta)
}

// ApplyRecordTags mirrors CreateTags into the owning AMI config so
// DescribeImages observes tags added after registration. Non-ami ids, AMIs
// absent from this store, and AMIs the caller does not own are skipped; the
// generic tag store stays their record of truth.
func (s *ImageServiceImpl) ApplyRecordTags(input *ec2.CreateTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	merge := utils.MergeTagsMut(input)
	for _, res := range input.Resources {
		if res == nil || !strings.HasPrefix(*res, "ami-") {
			continue
		}
		if err := s.updateAMITags(context.Background(), *res, accountID, merge); err != nil {
			return err
		}
	}
	return nil
}

// RemoveRecordTags mirrors DeleteTags into the owning AMI config. Empty
// input.Tags clears all tags; a tag with a value deletes only on a value match
// (AWS-faithful), a nil value deletes unconditionally.
func (s *ImageServiceImpl) RemoveRecordTags(input *ec2.DeleteTagsInput, accountID string) error {
	if input == nil {
		return nil
	}
	remove := utils.RemoveTagsMut(input)
	for _, res := range input.Resources {
		if res == nil || !strings.HasPrefix(*res, "ami-") {
			continue
		}
		if err := s.updateAMITags(context.Background(), *res, accountID, remove); err != nil {
			return err
		}
	}
	return nil
}

// updateAMITags read-modify-writes the tag map of the AMI config identified by
// imageID. An AMI absent from this store or owned by another account is skipped
// (its tags are not this service's to mutate); a corrupt config propagates.
func (s *ImageServiceImpl) updateAMITags(ctx context.Context, imageID, accountID string, mut func(map[string]string)) error {
	meta, err := s.GetAMIConfig(ctx, imageID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil
		}
		return err
	}
	if err := s.checkAMIOwnership(meta, accountID); err != nil {
		if err.Error() == awserrors.ErrorUnauthorizedOperation {
			slog.Debug("updateAMITags: skipping AMI not owned by caller", "imageId", imageID)
			return nil
		}
		return err
	}
	if meta.Tags == nil {
		meta.Tags = map[string]string{}
	}
	mut(meta.Tags)
	return s.putAMIConfig(ctx, imageID, meta)
}

// checkAMIOwnership rejects cross-account and system-AMI mutations. Empty
// owner is ServerInternal (corrupt config) rather than a misleading 403.
func (s *ImageServiceImpl) checkAMIOwnership(meta ebsmetadata.AMI, accountID string) error {
	owner := meta.ImageOwnerAlias
	if owner == "" {
		slog.Error("checkAMIOwnership: AMI config has empty ImageOwnerAlias", "imageId", meta.ImageID)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if !utils.IsAccountID(owner) || owner != accountID {
		return errors.New(awserrors.ErrorUnauthorizedOperation)
	}
	return nil
}

// callerCanReadAMI: empty owner is invisible (corrupt); non-account owner is
// a system AMI visible to all; account owner is private to that account.
func callerCanReadAMI(meta ebsmetadata.AMI, accountID string) bool {
	owner := meta.ImageOwnerAlias
	if owner == "" {
		return false
	}
	if !utils.IsAccountID(owner) {
		return true
	}
	return owner == accountID
}

// loadAMIForMutation fetches the AMI and enforces ownership. Cross-account
// callers see UnauthorizedOperation, not NotFound (they already know the ID).
// No CAS: concurrent writers last-write-wins on the full struct.
func (s *ImageServiceImpl) loadAMIForMutation(ctx context.Context, imageID, accountID string) (ebsmetadata.AMI, error) {
	if !strings.HasPrefix(imageID, "ami-") {
		return ebsmetadata.AMI{}, errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	meta, err := s.GetAMIConfig(ctx, imageID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return ebsmetadata.AMI{}, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
		}
		slog.Error("loadAMIForMutation: failed to read AMI config", "imageId", imageID, "err", err)
		return ebsmetadata.AMI{}, errors.New(awserrors.ErrorServerInternal)
	}

	if err := s.checkAMIOwnership(meta, accountID); err != nil {
		return ebsmetadata.AMI{}, err
	}
	return meta, nil
}

// putSnapshotMetadata stores the snapshot's control-plane document under the
// account that owns it.
func (s *ImageServiceImpl) putSnapshotMetadata(ctx context.Context, snapshotID, volumeID string, volumeSizeGiB uint64, accountID string) error {
	return s.metadata.PutSnapshot(ctx, ebsmetadata.Snapshot{
		SnapshotID: snapshotID,
		VolumeID:   volumeID,
		VolumeSize: safecast.Uint64ToInt64(volumeSizeGiB),
		State:      "completed",
		Progress:   "100%",
		StartTime:  time.Now(),
		OwnerID:    accountID,
	})
}

// CopyImage clones an AMI same-region, metadata-only: the new snapshot shares the
// source's VolumeID and a fresh config.json points at it. Source visibility is checked
// before the name-uniqueness scan so cross-account sources fast-fail.
func (s *ImageServiceImpl) CopyImage(ctx context.Context, input *ec2.CopyImageInput, accountID string) (*ec2.CopyImageOutput, error) {
	if input == nil || input.Name == nil || *input.Name == "" ||
		input.SourceImageId == nil || *input.SourceImageId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	name := *input.Name
	sourceImageID := *input.SourceImageId

	srcMeta, err := s.GetAMIConfig(ctx, sourceImageID)
	if err != nil {
		// Corrupt source is treated as NotFound so callers can't tell which
		// half of the AMI/snapshot pair is broken.
		if objectstore.IsNoSuchKeyError(err) || errors.Is(err, ebsmetadata.ErrCorruptDocument) {
			return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
		}
		slog.ErrorContext(ctx, "CopyImage: failed to read source AMI config", "sourceImageId", sourceImageID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if !callerCanReadAMI(srcMeta, accountID) {
		return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	}

	// Orphaned source (missing SnapshotID, or snapshot gone/corrupt) is
	// reported as NotFound — don't leak the half-broken state.
	if srcMeta.SnapshotID == "" {
		return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	}
	// Read the source snapshot under its own owner, not the caller's: a system
	// AMI's snapshot is keyed under the global account.
	srcSnapAccount, srcSnapDerived := snapshotAccountForAMI(srcMeta)
	srcSnap, err := s.metadata.GetSnapshot(ctx, srcSnapAccount, srcMeta.SnapshotID)
	if err != nil {
		// Bundled system AMIs have no standalone snapshot document; synthesize a
		// minimal view using VolumeID = sourceImageID so CopyImage succeeds.
		if objectstore.IsNoSuchKeyError(err) && srcSnapDerived {
			srcSnap = ebsmetadata.Snapshot{
				SnapshotID: srcMeta.SnapshotID,
				VolumeID:   sourceImageID,
				VolumeSize: safecast.Uint64ToInt64(srcMeta.VolumeSizeGiB),
			}
		} else if objectstore.IsNoSuchKeyError(err) || errors.Is(err, ebsmetadata.ErrCorruptDocument) {
			return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
		} else {
			slog.ErrorContext(ctx, "CopyImage: failed to read source snapshot metadata",
				"sourceImageId", sourceImageID, "snapshotId", srcMeta.SnapshotID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
	}

	if exists, err := s.amiNameExists(ctx, name); err != nil {
		slog.ErrorContext(ctx, "CopyImage: failed to check AMI name uniqueness", "name", name, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	} else if exists {
		return nil, errors.New(awserrors.ErrorInvalidAMINameDuplicate)
	}

	newSnapshotID := utils.GenerateResourceID("snap")
	newImageID := utils.GenerateResourceID("ami")

	// New snap shares source VolumeID — no block copy.
	snapSizeGiB := uint64(0)
	if srcSnap.VolumeSize > 0 {
		snapSizeGiB = uint64(srcSnap.VolumeSize)
	}
	if err := s.putSnapshotMetadata(ctx, newSnapshotID, srcSnap.VolumeID, snapSizeGiB, accountID); err != nil {
		slog.ErrorContext(ctx, "CopyImage: failed to write snapshot metadata", "snapshotId", newSnapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	description := srcMeta.Description
	if input.Description != nil {
		description = *input.Description
	}

	rootDeviceType := srcMeta.RootDeviceType
	if rootDeviceType == "" {
		rootDeviceType = "ebs"
	}

	tags := mergeCopyImageTags(srcMeta.Tags, input.TagSpecifications, aws.BoolValue(input.CopyImageTags))

	meta := ebsmetadata.AMI{
		ImageID:         newImageID,
		Name:            name,
		Description:     description,
		SnapshotID:      newSnapshotID,
		Architecture:    srcMeta.Architecture,
		PlatformDetails: srcMeta.PlatformDetails,
		Virtualization:  srcMeta.Virtualization,
		VolumeSizeGiB:   srcMeta.VolumeSizeGiB,
		RootDeviceType:  rootDeviceType,
		ImageOwnerAlias: accountID,
		CreationDate:    time.Now(),
		BootMode:        srcMeta.BootMode,
		Tags:            tags,
		// Zero-copy: the new config shares the source's already-durable
		// snapshot, so the image is complete as soon as this is written.
		State: "available",
	}

	if err := s.putAMIConfig(ctx, newImageID, meta); err != nil {
		slog.ErrorContext(ctx, "CopyImage: failed to write AMI config",
			"amiId", newImageID, "orphanSnapshotId", newSnapshotID, "err", err)
		// Best-effort rollback of the orphaned snapshot metadata.
		if delErr := s.metadata.DeleteSnapshot(ctx, accountID, newSnapshotID); delErr != nil {
			slog.ErrorContext(ctx, "CopyImage: failed to roll back orphaned snapshot metadata",
				"snapshotId", newSnapshotID, "err", delErr)
		}
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "CopyImage completed",
		"sourceImageId", sourceImageID, "newImageId", newImageID,
		"sourceSnapshotId", srcMeta.SnapshotID, "newSnapshotId", newSnapshotID,
		"accountId", accountID)

	return &ec2.CopyImageOutput{ImageId: aws.String(newImageID)}, nil
}

// mergeCopyImageTags seeds with source tags when copyImageTags is true, then
// lets image-resource TagSpecifications override colliding keys. Non-image tag
// specs are ignored.
func mergeCopyImageTags(srcTags map[string]string, specs []*ec2.TagSpecification, copyImageTags bool) map[string]string {
	merged := make(map[string]string)
	if copyImageTags {
		maps.Copy(merged, srcTags)
	}
	maps.Copy(merged, utils.ExtractTags(specs, "image"))
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// DescribeImageAttribute supports description and blockDeviceMapping only.
// Cross-account reads return NotFound so the caller can't learn the ID exists
// in another account.
func (s *ImageServiceImpl) DescribeImageAttribute(ctx context.Context, input *ec2.DescribeImageAttributeInput, accountID string) (*ec2.DescribeImageAttributeOutput, error) {
	if input == nil || input.ImageId == nil || *input.ImageId == "" ||
		input.Attribute == nil || *input.Attribute == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	imageID := *input.ImageId
	attribute := *input.Attribute

	meta, err := s.GetAMIConfig(ctx, imageID)
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
		}
		slog.ErrorContext(ctx, "DescribeImageAttribute: failed to read AMI config", "imageId", imageID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	if !callerCanReadAMI(meta, accountID) {
		return nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	}

	output := &ec2.DescribeImageAttributeOutput{
		ImageId: aws.String(imageID),
	}

	switch attribute {
	case ec2.ImageAttributeNameDescription:
		output.Description = &ec2.AttributeValue{Value: aws.String(meta.Description)}
	case ec2.ImageAttributeNameBlockDeviceMapping:
		output.BlockDeviceMappings = synthesizeRootBlockDeviceMapping(meta, s.clusterEncryptionEnabled())
	default:
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	slog.InfoContext(ctx, "DescribeImageAttribute completed", "imageId", imageID, "attribute", attribute, "accountId", accountID)
	return output, nil
}

// clusterEncryptionEnabled reports whether the daemon has a viperblock master
// key configured. AMI metadata carries no per-image encryption flag, so block
// device synthesis falls back to this cluster-level posture.
func (s *ImageServiceImpl) clusterEncryptionEnabled() bool {
	if s.config == nil {
		return false
	}
	mkey, err := utils.LoadViperblockMasterKey(s.config.Viperblock.EncryptionKeyFile)
	if err != nil {
		slog.Warn("clusterEncryptionEnabled: failed to load master key, reporting false", "err", err)
		return false
	}
	return mkey != nil
}

// synthesizeRootBlockDeviceMapping returns /dev/sda1 with size+snapshot from AMIMetadata,
// or nil for non-EBS AMIs. encrypted reflects the cluster-level encryption posture
// (master key configured); AMI metadata carries no per-image encryption flag.
func synthesizeRootBlockDeviceMapping(meta ebsmetadata.AMI, encrypted bool) []*ec2.BlockDeviceMapping {
	if meta.RootDeviceType != "ebs" {
		return nil
	}
	ebs := &ec2.EbsBlockDevice{
		VolumeSize:          aws.Int64(safecast.Uint64ToInt64(meta.VolumeSizeGiB)),
		VolumeType:          aws.String("gp3"),
		DeleteOnTermination: aws.Bool(true),
		Encrypted:           aws.Bool(encrypted),
	}
	if meta.SnapshotID != "" {
		ebs.SnapshotId = aws.String(meta.SnapshotID)
	}
	return []*ec2.BlockDeviceMapping{{
		DeviceName: aws.String("/dev/sda1"),
		Ebs:        ebs,
	}}
}

// RegisterImage writes AMI metadata pointing at an existing snapshot. Never
// touches block data. The snapshot read runs before the O(n) name-uniqueness
// scan so a missing snapshot fast-fails.
func (s *ImageServiceImpl) RegisterImage(ctx context.Context, input *ec2.RegisterImageInput, accountID string) (*ec2.RegisterImageOutput, error) {
	if input == nil || input.Name == nil || *input.Name == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	// The snapshot is read under the caller's account, so an untenanted caller
	// has no prefix to read from. Refuse at the boundary rather than in the key
	// builder, where the rejection would surface as an internal error.
	if accountID == "" {
		return nil, errors.New(awserrors.ErrorAuthFailure)
	}

	name := *input.Name

	rootBDM := pickRootSnapshotBDM(input.BlockDeviceMappings, input.RootDeviceName)
	if rootBDM == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	snapshotID := *rootBDM.Ebs.SnapshotId

	// The snapshot arrives through a caller-supplied BlockDeviceMapping, not
	// through an AMI, so there is no owner alias to derive from: it is read
	// under the caller's own account and the key is the ownership check.
	snapCfg, err := s.metadata.GetSnapshot(ctx, accountID, snapshotID)
	if err != nil {
		// Corrupt snapshot is surfaced as NotFound, same as CopyImage.
		if objectstore.IsNoSuchKeyError(err) || errors.Is(err, ebsmetadata.ErrCorruptDocument) {
			return nil, errors.New(awserrors.ErrorInvalidSnapshotNotFound)
		}
		slog.ErrorContext(ctx, "RegisterImage: failed to read snapshot metadata", "snapshotId", snapshotID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	snapSizeGiB := uint64(0)
	if snapCfg.VolumeSize > 0 {
		snapSizeGiB = uint64(snapCfg.VolumeSize)
	}

	volumeSizeGiB := snapSizeGiB
	if rootBDM.Ebs.VolumeSize != nil && *rootBDM.Ebs.VolumeSize > 0 {
		requested := uint64(*rootBDM.Ebs.VolumeSize)
		if requested < snapSizeGiB {
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		volumeSizeGiB = requested
	}

	if exists, err := s.amiNameExists(ctx, name); err != nil {
		slog.ErrorContext(ctx, "RegisterImage: failed to check AMI name uniqueness", "name", name, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	} else if exists {
		return nil, errors.New(awserrors.ErrorInvalidAMINameDuplicate)
	}

	architecture := "x86_64"
	if input.Architecture != nil && *input.Architecture != "" {
		architecture = *input.Architecture
	}
	virtualization := "hvm"
	if input.VirtualizationType != nil && *input.VirtualizationType != "" {
		virtualization = *input.VirtualizationType
	}
	description := ""
	if input.Description != nil {
		description = *input.Description
	}

	tags := utils.ExtractTags(input.TagSpecifications, "image")

	amiID := utils.GenerateResourceID("ami")
	meta := ebsmetadata.AMI{
		ImageID:         amiID,
		Name:            name,
		Description:     description,
		SnapshotID:      snapshotID,
		Architecture:    architecture,
		PlatformDetails: "Linux/UNIX",
		Virtualization:  virtualization,
		VolumeSizeGiB:   volumeSizeGiB,
		RootDeviceType:  "ebs",
		ImageOwnerAlias: accountID,
		CreationDate:    time.Now(),
		Tags:            tags,
		BootMode:        aws.StringValue(input.BootMode),
		// The referenced snapshot already exists (checked above), so the
		// image is complete as soon as this config is written.
		State: "available",
	}

	if err := s.putAMIConfig(ctx, amiID, meta); err != nil {
		slog.ErrorContext(ctx, "RegisterImage: failed to write AMI config", "amiId", amiID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "RegisterImage completed", "amiId", amiID, "snapshotId", snapshotID, "accountId", accountID)
	return &ec2.RegisterImageOutput{ImageId: aws.String(amiID)}, nil
}

// pickRootSnapshotBDM returns the BDM matching RootDeviceName (if set) that
// carries a non-empty EBS snapshot reference, else the first such BDM.
func pickRootSnapshotBDM(mappings []*ec2.BlockDeviceMapping, rootDeviceName *string) *ec2.BlockDeviceMapping {
	wantName := ""
	if rootDeviceName != nil {
		wantName = *rootDeviceName
	}

	for _, bdm := range mappings {
		if bdm == nil || bdm.Ebs == nil || bdm.Ebs.SnapshotId == nil || *bdm.Ebs.SnapshotId == "" {
			continue
		}
		if wantName != "" {
			if bdm.DeviceName == nil || *bdm.DeviceName != wantName {
				continue
			}
		}
		return bdm
	}
	return nil
}

// DeregisterImage hard-deletes the AMI document. The pre-provider
// {imageID}/config.json goes too, or the metadata store's legacy read fallback
// would resurrect the image. Backing snapshot is untouched, so operators run
// delete-snapshot separately to reclaim block storage.
func (s *ImageServiceImpl) DeregisterImage(ctx context.Context, input *ec2.DeregisterImageInput, accountID string) (*ec2.DeregisterImageOutput, error) {
	if input == nil || input.ImageId == nil || *input.ImageId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	imageID := *input.ImageId

	if _, err := s.loadAMIForMutation(ctx, imageID, accountID); err != nil {
		return nil, err
	}

	if err := s.metadata.DeleteAMI(ctx, imageID); err != nil {
		slog.ErrorContext(ctx, "DeregisterImage: failed to delete AMI document", "imageId", imageID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	configKey := fmt.Sprintf("%s/config.json", imageID)
	if _, err := s.store.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(configKey),
	}); err != nil && !objectstore.IsNoSuchKeyError(err) {
		slog.ErrorContext(ctx, "DeregisterImage: failed to delete legacy AMI config", "imageId", imageID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "DeregisterImage completed", "imageId", imageID, "accountId", accountID)
	return &ec2.DeregisterImageOutput{}, nil
}

// ModifyImageAttribute writes a modifiable AMI attribute; only description is writable.
// Ownership is checked first so cross-account callers always see UnauthorizedOperation.
func (s *ImageServiceImpl) ModifyImageAttribute(ctx context.Context, input *ec2.ModifyImageAttributeInput, accountID string) (*ec2.ModifyImageAttributeOutput, error) {
	if input == nil || input.ImageId == nil || *input.ImageId == "" ||
		input.Attribute == nil || *input.Attribute == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	imageID := *input.ImageId
	attribute := *input.Attribute

	meta, err := s.loadAMIForMutation(ctx, imageID, accountID)
	if err != nil {
		return nil, err
	}

	if attribute != ec2.ImageAttributeNameDescription {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	newValue := ""
	if input.Value != nil {
		newValue = *input.Value
	}
	// No-op guard: Terraform aws_ami refresh otherwise churns out no-op writes.
	if meta.Description == newValue {
		slog.InfoContext(ctx, "ModifyImageAttribute no-op", "imageId", imageID, "attribute", attribute, "accountId", accountID)
		return &ec2.ModifyImageAttributeOutput{}, nil
	}
	meta.Description = newValue

	if err := s.putAMIConfig(ctx, imageID, meta); err != nil {
		slog.ErrorContext(ctx, "ModifyImageAttribute: failed to write AMI config", "imageId", imageID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "ModifyImageAttribute completed", "imageId", imageID, "attribute", attribute, "accountId", accountID)
	return &ec2.ModifyImageAttributeOutput{}, nil
}

// ResetImageAttribute clears the description (the only supported attribute).
// launchPermission — AWS's default reset target — is out of scope.
func (s *ImageServiceImpl) ResetImageAttribute(ctx context.Context, input *ec2.ResetImageAttributeInput, accountID string) (*ec2.ResetImageAttributeOutput, error) {
	if input == nil || input.ImageId == nil || *input.ImageId == "" ||
		input.Attribute == nil || *input.Attribute == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	imageID := *input.ImageId
	attribute := *input.Attribute

	meta, err := s.loadAMIForMutation(ctx, imageID, accountID)
	if err != nil {
		return nil, err
	}

	if attribute != ec2.ImageAttributeNameDescription {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if meta.Description == "" {
		slog.InfoContext(ctx, "ResetImageAttribute no-op", "imageId", imageID, "attribute", attribute, "accountId", accountID)
		return &ec2.ResetImageAttributeOutput{}, nil
	}
	meta.Description = ""

	if err := s.putAMIConfig(ctx, imageID, meta); err != nil {
		slog.ErrorContext(ctx, "ResetImageAttribute: failed to write AMI config", "imageId", imageID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.InfoContext(ctx, "ResetImageAttribute completed", "imageId", imageID, "attribute", attribute, "accountId", accountID)
	return &ec2.ResetImageAttributeOutput{}, nil
}
