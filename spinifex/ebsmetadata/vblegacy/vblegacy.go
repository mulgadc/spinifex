// Package vblegacy converts between viperblock's persisted metadata schema
// and the spinifex-owned ebsmetadata documents.
//
// It sits beside ebsmetadata rather than inside it because ebsmetadata must
// not import viperblock. Every caller that needs the mapping imports this,
// so a field added to either schema has exactly one place to be wired up.
package vblegacy

import (
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/viperblock/viperblock"
)

// AMIToDocument converts viperblock's AMI metadata to the control-plane
// document. The two schemas map field for field.
func AMIToDocument(meta viperblock.AMIMetadata) ebsmetadata.AMI {
	return ebsmetadata.AMI{
		ImageID:         meta.ImageID,
		Name:            meta.Name,
		Description:     meta.Description,
		Architecture:    meta.Architecture,
		PlatformDetails: meta.PlatformDetails,
		CreationDate:    meta.CreationDate,
		RootDeviceType:  meta.RootDeviceType,
		Virtualization:  meta.Virtualization,
		ImageOwnerAlias: meta.ImageOwnerAlias,
		VolumeSizeGiB:   meta.VolumeSizeGiB,
		SnapshotID:      meta.SnapshotID,
		BootMode:        meta.BootMode,
		Distro:          meta.Distro,
		DistroFamily:    meta.DistroFamily,
		Tags:            meta.Tags,
		State:           meta.State,
	}
}

// AMIFromDocument is AMIToDocument's inverse, used by the embedded path to
// preserve the legacy config.json shape.
func AMIFromDocument(ami ebsmetadata.AMI) viperblock.AMIMetadata {
	return viperblock.AMIMetadata{
		ImageID:         ami.ImageID,
		Name:            ami.Name,
		Description:     ami.Description,
		Architecture:    ami.Architecture,
		PlatformDetails: ami.PlatformDetails,
		CreationDate:    ami.CreationDate,
		RootDeviceType:  ami.RootDeviceType,
		Virtualization:  ami.Virtualization,
		ImageOwnerAlias: ami.ImageOwnerAlias,
		VolumeSizeGiB:   ami.VolumeSizeGiB,
		SnapshotID:      ami.SnapshotID,
		BootMode:        ami.BootMode,
		Distro:          ami.Distro,
		DistroFamily:    ami.DistroFamily,
		Tags:            ami.Tags,
		State:           ami.State,
	}
}
