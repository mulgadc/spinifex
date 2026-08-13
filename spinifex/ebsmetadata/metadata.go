// Package ebsmetadata owns Spinifex's control-plane view of EBS resources.
// These documents deliberately do not mirror viperblock's VBState schema.
package ebsmetadata

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const SchemaVersion uint16 = 1

// ErrCorruptDocument wraps decode and schema-version failures so callers can
// tell a document that exists but cannot be read from one that is absent. The
// two deserve different answers: salvage versus not-found.
var ErrCorruptDocument = errors.New("corrupt EBS metadata document")

// Volume is the control-plane record used for EC2 Describe/Modify/attachment
// operations. ProviderHandle is opaque and is never decoded by the API layer.
type Volume struct {
	SchemaVersion       uint16              `json:"schema_version"`
	VolumeID            string              `json:"volume_id"`
	VolumeName          string              `json:"volume_name,omitempty"`
	TenantID            string              `json:"tenant_id"`
	CapacityGiB         uint64              `json:"capacity_gib"`
	State               string              `json:"state"`
	CreatedAt           time.Time           `json:"created_at"`
	AttachedAt          time.Time           `json:"attached_at,omitzero"`
	AvailabilityZone    string              `json:"availability_zone"`
	AttachedInstance    string              `json:"attached_instance,omitempty"`
	DeviceName          string              `json:"device_name,omitempty"`
	VolumeType          string              `json:"volume_type"`
	IOPS                int                 `json:"iops"`
	Throughput          int                 `json:"throughput,omitempty"`
	Tags                map[string]string   `json:"tags,omitempty"`
	SnapshotID          string              `json:"snapshot_id,omitempty"`
	DeleteOnTermination bool                `json:"delete_on_termination,omitempty"`
	Encrypted           bool                `json:"encrypted,omitempty"`
	ProviderHandle      string              `json:"provider_handle,omitempty"`
	Modification        *VolumeModification `json:"modification,omitempty"`
}

// VolumeModification is the control-plane record of a completed or in-flight
// ModifyVolume operation, read back by DescribeVolumesModifications. It owns
// its own fields rather than reusing viperblock.VolumeModification: this
// package must stay free of viperblock types. VolumeID is deliberately not
// duplicated here — callers have it from the owning Volume.
type VolumeModification struct {
	ModificationState  string    `json:"modification_state"`
	Progress           int64     `json:"progress"`
	StatusMessage      string    `json:"status_message,omitempty"`
	OriginalSize       int64     `json:"original_size"`
	OriginalIOPS       int64     `json:"original_iops"`
	OriginalVolumeType string    `json:"original_volume_type"`
	TargetSize         int64     `json:"target_size"`
	TargetIOPS         int64     `json:"target_iops"`
	TargetVolumeType   string    `json:"target_volume_type"`
	StartTime          time.Time `json:"start_time"`
	EndTime            time.Time `json:"end_time,omitzero"`
}

// AMI is the control-plane record used for EC2 image operations.
type AMI struct {
	SchemaVersion   uint16            `json:"schema_version"`
	ImageID         string            `json:"image_id"`
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	Architecture    string            `json:"architecture"`
	PlatformDetails string            `json:"platform_details"`
	CreationDate    time.Time         `json:"creation_date"`
	RootDeviceType  string            `json:"root_device_type"`
	Virtualization  string            `json:"virtualization"`
	ImageOwnerAlias string            `json:"image_owner_alias"`
	VolumeSizeGiB   uint64            `json:"volume_size_gib"`
	SnapshotID      string            `json:"snapshot_id,omitempty"`
	BootMode        string            `json:"boot_mode,omitempty"`
	Distro          string            `json:"distro,omitempty"`
	DistroFamily    string            `json:"distro_family,omitempty"`
	Tags            map[string]string `json:"tags,omitempty"`
	State           string            `json:"state,omitempty"`
}

func VolumeKey(volumeID string) (string, error) { return key("volumes", volumeID) }
func AMIKey(imageID string) (string, error)     { return key("amis", imageID) }

func key(kind, id string) (string, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") {
		return "", fmt.Errorf("invalid EBS metadata ID %q", id)
	}
	return "spinifex/ebsmetadata/v1/" + kind + "/" + id + ".json", nil
}

func MarshalVolume(volume Volume) ([]byte, error) {
	volume.SchemaVersion = SchemaVersion
	return json.Marshal(volume)
}

func UnmarshalVolume(data []byte) (Volume, error) {
	var volume Volume
	if err := json.Unmarshal(data, &volume); err != nil {
		return Volume{}, fmt.Errorf("%w: %w", ErrCorruptDocument, err)
	}
	if volume.SchemaVersion != SchemaVersion {
		return Volume{}, fmt.Errorf("%w: unsupported volume metadata schema version %d", ErrCorruptDocument, volume.SchemaVersion)
	}
	return volume, nil
}

func MarshalAMI(ami AMI) ([]byte, error) {
	ami.SchemaVersion = SchemaVersion
	return json.Marshal(ami)
}

func UnmarshalAMI(data []byte) (AMI, error) {
	var ami AMI
	if err := json.Unmarshal(data, &ami); err != nil {
		return AMI{}, fmt.Errorf("%w: %w", ErrCorruptDocument, err)
	}
	if ami.SchemaVersion != SchemaVersion {
		return AMI{}, fmt.Errorf("%w: unsupported AMI metadata schema version %d", ErrCorruptDocument, ami.SchemaVersion)
	}
	return ami, nil
}
