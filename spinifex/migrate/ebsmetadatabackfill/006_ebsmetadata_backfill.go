// Package ebsmetadatabackfill converts volumes and AMIs created under the
// legacy embedded EBS engine into spinifex-owned ebsmetadata documents.
//
// It sits outside the migrate package because it needs viperblock's VBState
// schema to read the legacy config.json shape, and migrate is imported by
// every handler that stamps a KV bucket version — a heavy dependency there
// would reach the whole tree (see migrate/predastoretopology, which exists
// for the same reason). Only the daemon, which already depends on viperblock
// through the embedded EBS engine, imports this package.
package ebsmetadatabackfill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata/vblegacy"
	"github.com/mulgadc/spinifex/spinifex/handlers/ec2/volumestate"
	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/viperblock/viperblock"
)

// MigrationTarget names this migration in the migrate registry and is the
// target string daemon.go passes to Registry.RunObject.
const MigrationTarget = "ebsmetadata"

// TargetVersion is the version daemon.go migrates the target to.
const TargetVersion = 1

func init() {
	migrate.DefaultRegistry.RegisterObject(MigrationTarget, migrate.ObjectMigration{
		FromVersion: 0,
		ToVersion:   TargetVersion,
		Description: "backfill spinifex/ebsmetadata/v1 documents from legacy viperblock config.json",
		Run:         backfill,
	})
}

// backfill converts every legacy volume and AMI it can read into an
// ebsmetadata document, skipping and logging unreadable ones: one corrupt
// volume must not hide every other volume from the control plane.
//
// Re-running overwrites each document with identical content, so two nodes
// racing this step is harmless.
func backfill(ctx context.Context, octx migrate.ObjectContext) error {
	volumesConverted, volumesSkipped, err := backfillVolumes(ctx, octx)
	if err != nil {
		return err
	}
	amisConverted, amisSkipped, err := backfillAMIs(ctx, octx)
	if err != nil {
		return err
	}
	octx.Logger.Info("ebsmetadata backfill complete",
		"volumes_converted", volumesConverted, "volumes_skipped", volumesSkipped,
		"amis_converted", amisConverted, "amis_skipped", amisSkipped)
	return nil
}

func backfillVolumes(ctx context.Context, octx migrate.ObjectContext) (converted, skipped int, err error) {
	ids, err := ebsmetadata.LegacyPrefixIDs(ctx, octx.Objects, octx.Bucket, "vol-", "-efi", "-cloudinit")
	if err != nil {
		return 0, 0, fmt.Errorf("list legacy volume prefixes: %w", err)
	}

	store := ebsmetadata.NewStore(octx.Objects, octx.Bucket)
	for _, volumeID := range ids {
		volume, found, convertErr := LegacyVolumeFromLegacyState(ctx, octx.Objects, octx.Bucket, volumeID)
		if convertErr != nil {
			octx.Logger.Warn("skipping unreadable legacy volume during ebsmetadata backfill", "volumeId", volumeID, "err", convertErr)
			skipped++
			continue
		}
		if !found {
			// Listed as a prefix but config.json vanished between list and
			// read (deleted concurrently); nothing to convert.
			continue
		}
		if err := store.PutVolume(ctx, volume); err != nil {
			return converted, skipped, fmt.Errorf("write ebsmetadata volume %s: %w", volumeID, err)
		}
		converted++
	}
	return converted, skipped, nil
}

func backfillAMIs(ctx context.Context, octx migrate.ObjectContext) (converted, skipped int, err error) {
	ids, err := ebsmetadata.LegacyPrefixIDs(ctx, octx.Objects, octx.Bucket, "ami-")
	if err != nil {
		return 0, 0, fmt.Errorf("list legacy AMI prefixes: %w", err)
	}

	store := ebsmetadata.NewStore(octx.Objects, octx.Bucket)
	for _, imageID := range ids {
		ami, found, convertErr := LegacyAMIFromLegacyState(ctx, octx.Objects, octx.Bucket, imageID)
		if convertErr != nil {
			octx.Logger.Warn("skipping unreadable legacy AMI during ebsmetadata backfill", "imageId", imageID, "err", convertErr)
			skipped++
			continue
		}
		if !found {
			continue
		}
		if err := store.PutAMI(ctx, ami); err != nil {
			return converted, skipped, fmt.Errorf("write ebsmetadata AMI %s: %w", imageID, err)
		}
		converted++
	}
	return converted, skipped, nil
}

// volumeConfigWrapper matches the JSON structure stored in S3 config.json
// files that were never through a full VBState SaveState (e.g. CreateVolume
// pre-mount). Mirrors handlers/ec2/volume's private wrapper of the same shape.
type volumeConfigWrapper struct {
	VolumeConfig viperblock.VolumeConfig `json:"VolumeConfig"`
}

// volumeTagsKey is the S3 key for a volume's control-plane tags object.
func volumeTagsKey(volumeID string) string { return volumeID + "/tags.json" }

// LegacyVolumeFromLegacyState reads a volume's legacy config.json (plus the
// control-plane-owned state.json/tags.json overlays) and converts it to an
// ebsmetadata.Volume. found=false with a nil error means volumeID has no
// legacy record either. It is shared by the backfill migration and by
// ebsmetadata.Store's read-path fallback (see SetLegacyVolumeFallback), so
// the decode logic exists in exactly one place.
func LegacyVolumeFromLegacyState(ctx context.Context, objects objectstore.ObjectStore, bucket, volumeID string) (ebsmetadata.Volume, bool, error) {
	vc, encrypted, found, err := readBaseVolumeConfig(ctx, objects, bucket, volumeID)
	if err != nil || !found {
		return ebsmetadata.Volume{}, found, err
	}

	// state.json is authoritative over the State/attachment fields embedded in
	// config.json, which the live nbdkit VB rewrites on every SaveState.
	if rec, found, err := volumestate.Read(ctx, objects, bucket, volumeID); err != nil {
		return ebsmetadata.Volume{}, false, fmt.Errorf("read volume state for %s: %w", volumeID, err)
	} else if found {
		vc.VolumeMetadata.State = rec.State
		vc.VolumeMetadata.AttachedInstance = rec.AttachedInstance
		vc.VolumeMetadata.DeviceName = rec.DeviceName
		vc.VolumeMetadata.AttachedAt = rec.AttachedAt
	}

	// tags.json is authoritative over the Tags embedded in config.json,
	// including when it holds an empty map.
	if tags, found, err := readVolumeTags(ctx, objects, bucket, volumeID); err != nil {
		return ebsmetadata.Volume{}, false, fmt.Errorf("read volume tags for %s: %w", volumeID, err)
	} else if found {
		vc.VolumeMetadata.Tags = tags
	}

	meta := vc.VolumeMetadata
	if meta.VolumeID == "" {
		return ebsmetadata.Volume{}, false, fmt.Errorf("volume %s: config.json has an empty VolumeID", volumeID)
	}
	if meta.SizeGiB == 0 {
		return ebsmetadata.Volume{}, false, fmt.Errorf("volume %s: config.json has zero size", volumeID)
	}

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

	volume := ebsmetadata.Volume{
		VolumeID:            meta.VolumeID,
		VolumeName:          meta.VolumeName,
		TenantID:            meta.TenantID,
		CapacityGiB:         meta.SizeGiB,
		State:               state,
		CreatedAt:           meta.CreatedAt,
		AttachedAt:          meta.AttachedAt,
		AvailabilityZone:    meta.AvailabilityZone,
		AttachedInstance:    meta.AttachedInstance,
		DeviceName:          meta.DeviceName,
		VolumeType:          volumeType,
		IOPS:                meta.IOPS,
		Throughput:          meta.Throughput,
		Tags:                meta.Tags,
		SnapshotID:          meta.SnapshotID,
		DeleteOnTermination: meta.DeleteOnTermination,
		Encrypted:           encrypted,
	}
	if vc.Modification != nil {
		volume.Modification = &ebsmetadata.VolumeModification{
			ModificationState:  vc.Modification.ModificationState,
			Progress:           vc.Modification.Progress,
			StatusMessage:      vc.Modification.StatusMessage,
			OriginalSize:       vc.Modification.OriginalSize,
			OriginalIOPS:       vc.Modification.OriginalIops,
			OriginalVolumeType: vc.Modification.OriginalVolumeType,
			TargetSize:         vc.Modification.TargetSize,
			TargetIOPS:         vc.Modification.TargetIops,
			TargetVolumeType:   vc.Modification.TargetVolumeType,
			StartTime:          vc.Modification.StartTime,
			EndTime:            vc.Modification.EndTime,
		}
	}

	return volume, true, nil
}

// readBaseVolumeConfig reads config.json and decodes it without overlaying
// state.json/tags.json, handling both the sealed-at-rest-encryption envelope
// and the plain wrapper shape (mirrors
// handlers/ec2/volume's getBaseVolumeConfigAndEncryption).
func readBaseVolumeConfig(ctx context.Context, objects objectstore.ObjectStore, bucket, volumeID string) (*viperblock.VolumeConfig, bool, bool, error) {
	getResult, err := objects.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(volumeID + "/config.json"),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, false, false, nil
		}
		return nil, false, false, fmt.Errorf("get config for %s: %w", volumeID, err)
	}
	defer getResult.Body.Close()

	body, err := io.ReadAll(getResult.Body)
	if err != nil {
		return nil, false, false, fmt.Errorf("read config body for %s: %w", volumeID, err)
	}
	// Unwrap the at-rest encryption envelope; no-op for unencrypted volumes.
	body = viperblock.StateBody(body)

	var state viperblock.VBState
	if decodeErr := json.NewDecoder(bytes.NewReader(body)).Decode(&state); decodeErr == nil && state.BlockSize != 0 {
		return &state.VolumeConfig, state.EncryptionEnabled, true, nil
	}

	var wrapper volumeConfigWrapper
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, false, false, fmt.Errorf("unmarshal config for %s: %w", volumeID, err)
	}
	return &wrapper.VolumeConfig, false, true, nil
}

// readVolumeTags reads tags.json. found=false means the object is absent; a
// present empty map is an authoritative empty tag set.
func readVolumeTags(ctx context.Context, objects objectstore.ObjectStore, bucket, volumeID string) (map[string]string, bool, error) {
	result, err := objects.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(volumeTagsKey(volumeID)),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("get tags: %w", err)
	}
	defer result.Body.Close()

	body, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, false, fmt.Errorf("read tags body: %w", err)
	}
	var tags map[string]string
	if err := json.Unmarshal(body, &tags); err != nil {
		return nil, false, fmt.Errorf("unmarshal tags: %w", err)
	}
	if tags == nil {
		return nil, false, fmt.Errorf("unmarshal tags: expected JSON object")
	}
	return tags, true, nil
}

// LegacyAMIFromLegacyState reads an AMI's legacy config.json and converts it
// to an ebsmetadata.AMI. found=false with a nil error means imageID has no
// legacy record either. Shared by the backfill migration and by
// ebsmetadata.Store's read-path fallback (see SetLegacyAMIFallback).
func LegacyAMIFromLegacyState(ctx context.Context, objects objectstore.ObjectStore, bucket, imageID string) (ebsmetadata.AMI, bool, error) {
	getResult, err := objects.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(imageID + "/config.json"),
	})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return ebsmetadata.AMI{}, false, nil
		}
		return ebsmetadata.AMI{}, false, fmt.Errorf("get config for %s: %w", imageID, err)
	}
	defer getResult.Body.Close()

	body, err := io.ReadAll(getResult.Body)
	if err != nil {
		return ebsmetadata.AMI{}, false, fmt.Errorf("read config body for %s: %w", imageID, err)
	}

	var vbState viperblock.VBState
	if err := json.Unmarshal(viperblock.StateBody(body), &vbState); err != nil {
		return ebsmetadata.AMI{}, false, fmt.Errorf("unmarshal config for %s: %w", imageID, err)
	}

	meta := vbState.VolumeConfig.AMIMetadata
	if meta.ImageID == "" {
		return ebsmetadata.AMI{}, false, fmt.Errorf("AMI %s: config.json has an empty ImageID", imageID)
	}

	return vblegacy.AMIToDocument(meta), true, nil
}
