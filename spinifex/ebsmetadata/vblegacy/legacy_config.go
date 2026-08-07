package vblegacy

import "time"

// VBState mirrors viperblock.VBState field-for-field, including JSON tags and
// omitempty presence, so callers can build or decode the legacy config.json
// document viperblock's embedded engine writes without importing viperblock
// themselves. legacy_config_equivalence_test.go guards this mirror against
// drift; if a field is added to the real type here without changing that
// test's fixture, the test does not catch it, so keep the two aligned by hand.
type VBState struct {
	VolumeName string `json:"VolumeName"`
	VolumeSize uint64 `json:"VolumeSize"`

	BlockSize    uint32 `json:"BlockSize"`
	ObjBlockSize uint32 `json:"ObjBlockSize"`

	SeqNum    uint64 `json:"SeqNum"`
	ObjectNum uint64 `json:"ObjectNum"`
	WALNum    uint64 `json:"WALNum"`

	BlockToObjectWALNum uint64 `json:"BlockToObjectWALNum"`

	Version uint16 `json:"Version"`

	VolumeConfig VolumeConfig `json:"VolumeConfig"`

	ShardedWAL bool `json:"ShardedWAL,omitempty"`

	SnapshotID       string `json:"SnapshotID,omitempty"`
	SourceVolumeName string `json:"SourceVolumeName,omitempty"`

	EncryptionEnabled bool    `json:"EncryptionEnabled,omitempty"`
	VolumeUUID        [4]byte `json:"VolumeUUID,omitempty"`
	SeqNumHighWater   uint64  `json:"SeqNumHighWater,omitempty"`
	StateSeqNum       uint64  `json:"StateSeqNum,omitempty"`
	KeyFingerprint    string  `json:"KeyFingerprint,omitempty"`
}

// VolumeConfig mirrors viperblock.VolumeConfig.
type VolumeConfig struct {
	VolumeMetadata VolumeMetadata      `json:"VolumeMetadata"`
	AMIMetadata    AMIMetadata         `json:"AMIMetadata"`
	Modification   *VolumeModification `json:"Modification,omitempty"`
}

// VolumeModification mirrors viperblock.VolumeModification.
type VolumeModification struct {
	VolumeID           string    `json:"VolumeID"`
	ModificationState  string    `json:"ModificationState"`
	Progress           int64     `json:"Progress"`
	StatusMessage      string    `json:"StatusMessage,omitempty"`
	OriginalSize       int64     `json:"OriginalSize"`
	OriginalIops       int64     `json:"OriginalIops"`
	OriginalVolumeType string    `json:"OriginalVolumeType"`
	TargetSize         int64     `json:"TargetSize"`
	TargetIops         int64     `json:"TargetIops"`
	TargetVolumeType   string    `json:"TargetVolumeType"`
	StartTime          time.Time `json:"StartTime"`
	EndTime            time.Time `json:"EndTime,omitzero"`
}

// VolumeMetadata mirrors viperblock.VolumeMetadata.
type VolumeMetadata struct {
	VolumeID            string            `json:"VolumeID"`
	VolumeName          string            `json:"VolumeName"`
	TenantID            string            `json:"TenantID"`
	SizeGiB             uint64            `json:"SizeGiB"`
	State               string            `json:"State"`
	CreatedAt           time.Time         `json:"CreatedAt"`
	AttachedAt          time.Time         `json:"AttachedAt"`
	AvailabilityZone    string            `json:"AvailabilityZone"`
	AttachedInstance    string            `json:"AttachedInstance"`
	DeviceName          string            `json:"DeviceName"`
	VolumeType          string            `json:"VolumeType"`
	IOPS                int               `json:"IOPS"`
	Throughput          int               `json:"Throughput"`
	Tags                map[string]string `json:"Tags"`
	SnapshotID          string            `json:"SnapshotID"`
	DeleteOnTermination bool              `json:"DeleteOnTermination"`
}

// AMIMetadata mirrors viperblock.AMIMetadata.
type AMIMetadata struct {
	ImageID         string            `json:"ImageID"`
	Name            string            `json:"Name"`
	Description     string            `json:"Description"`
	Architecture    string            `json:"Architecture"`
	PlatformDetails string            `json:"PlatformDetails"`
	CreationDate    time.Time         `json:"CreationDate"`
	RootDeviceType  string            `json:"RootDeviceType"`
	Virtualization  string            `json:"Virtualization"`
	ImageOwnerAlias string            `json:"ImageOwnerAlias"`
	VolumeSizeGiB   uint64            `json:"VolumeSizeGiB"`
	SnapshotID      string            `json:"SnapshotID"`
	BootMode        string            `json:"BootMode,omitempty"`
	Distro          string            `json:"Distro,omitempty"`
	DistroFamily    string            `json:"DistroFamily,omitempty"`
	Tags            map[string]string `json:"Tags"`
	State           string            `json:"State,omitempty"`
}

// ConfigWrapper mirrors the JSON shape spinifex itself writes to config.json
// before a full VBState exists (e.g. CreateVolume pre-mount) — just the
// VolumeConfig key, with no BlockSize/SeqNum/etc. Both
// handlers/ec2/volume's volumeConfigWrapper and
// migrate/ebsmetadatabackfill's volumeConfigWrapper are this same shape.
type ConfigWrapper struct {
	VolumeConfig VolumeConfig `json:"VolumeConfig"`
}
