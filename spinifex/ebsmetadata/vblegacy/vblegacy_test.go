package vblegacy

import (
	"reflect"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fullLegacyAMI() viperblock.AMIMetadata {
	return viperblock.AMIMetadata{
		ImageID:         "ami-123",
		Name:            "test-ami",
		Description:     "a description",
		Architecture:    "x86_64",
		PlatformDetails: "Linux/UNIX",
		CreationDate:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		RootDeviceType:  "ebs",
		Virtualization:  "hvm",
		ImageOwnerAlias: "self",
		VolumeSizeGiB:   8,
		SnapshotID:      "snap-123",
		BootMode:        "uefi",
		Distro:          "debian",
		DistroFamily:    "debian",
		Tags:            map[string]string{"Name": "test"},
		State:           "available",
	}
}

func TestAMIRoundTrip_PreservesEveryField(t *testing.T) {
	original := fullLegacyAMI()
	assert.Equal(t, original, AMIFromDocument(AMIToDocument(original)))
}

// TestAMIToDocument_CoversEveryLegacyField fails when a field is added to
// viperblock.AMIMetadata without being wired into the converters, which would
// otherwise drop it silently on every provider-path read and write.
func TestAMIToDocument_CoversEveryLegacyField(t *testing.T) {
	document := AMIToDocument(fullLegacyAMI())

	value := reflect.ValueOf(document)
	for i := range value.NumField() {
		name := value.Type().Field(i).Name
		if name == "SchemaVersion" {
			// Stamped by MarshalAMI, not carried on the legacy side.
			continue
		}
		require.Falsef(t, value.Field(i).IsZero(),
			"ebsmetadata.AMI.%s is zero after conversion: add it to AMIToDocument", name)
	}
}

func TestAMIFromDocument_CoversEveryDocumentField(t *testing.T) {
	document := AMIToDocument(fullLegacyAMI())
	legacy := AMIFromDocument(document)

	value := reflect.ValueOf(legacy)
	for i := range value.NumField() {
		name := value.Type().Field(i).Name
		require.Falsef(t, value.Field(i).IsZero(),
			"viperblock.AMIMetadata.%s is zero after conversion: add it to AMIFromDocument", name)
	}
}

// Guards the assumption the converters rest on: the document carries every
// legacy field plus its own schema stamp, so neither direction loses data.
func TestAMISchemasStayFieldCompatible(t *testing.T) {
	legacyFields := reflect.TypeFor[viperblock.AMIMetadata]().NumField()
	documentFields := reflect.TypeFor[ebsmetadata.AMI]().NumField()
	assert.Equal(t, legacyFields+1, documentFields,
		"AMI schemas drifted: reconcile them and update the converters")
}
