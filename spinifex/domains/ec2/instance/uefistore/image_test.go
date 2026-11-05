package uefistore_test

import (
	"testing"

	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const imageID = "ami-0123456789abcdef0"

// A launch only seeds from a store under the image's own prefix with a
// firmware it can check; an AMI with no reference seeds from the template.
func TestImageVarStoreFromAMI(t *testing.T) {
	fw := ebsmetadata.UEFIFirmware{Arch: "x86_64", VarsSize: 4096, CodeSHA256: "c"}
	own := uefistore.KeyPrefix(imageID) + "k"

	got, err := uefistore.ImageVarStoreFromAMI(ebsmetadata.AMI{ImageID: imageID})
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = uefistore.ImageVarStoreFromAMI(ebsmetadata.AMI{ImageID: imageID, UEFIData: &ebsmetadata.UEFIDataRef{Key: own, Firmware: fw}})
	require.NoError(t, err)
	assert.Equal(t, &uefistore.ImageVarStore{
		ImageID: imageID, Key: own, Firmware: uefistore.FirmwareIdentity{Arch: "x86_64", VarsSize: 4096, CodeSHA256: "c"},
	}, got)

	for name, ref := range map[string]ebsmetadata.UEFIDataRef{
		"another image's key":   {Key: uefistore.KeyPrefix("ami-0fffffffffffffff0") + "k", Firmware: fw},
		"an instance's key":     {Key: uefistore.KeyPrefix(instanceID) + "k", Firmware: fw},
		"a nested key":          {Key: own + "/deeper", Firmware: fw},
		"no firmware arch":      {Key: own, Firmware: ebsmetadata.UEFIFirmware{VarsSize: 4096}},
		"no firmware VARS size": {Key: own, Firmware: ebsmetadata.UEFIFirmware{Arch: "x86_64"}},
	} {
		_, err := uefistore.ImageVarStoreFromAMI(ebsmetadata.AMI{ImageID: imageID, UEFIData: &ref})
		require.ErrorIs(t, err, uefistore.ErrInvalidObject, name)
	}
}

// A copy is a fresh key under the new owner holding the source's region, and
// the source is left in place.
func TestCopyObject_PutsTheRegionAtAFreshKeyUnderTheNewOwner(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	meta := testMeta(instanceID)
	region := make([]byte, meta.Firmware.VarsSize)
	copy(region, "variables")
	src, err := uefistore.NewObjectKey(instanceID)
	require.NoError(t, err)
	require.NoError(t, uefistore.PutObject(t.Context(), objects, testBucket, src, region, meta))

	dst := testMeta(imageID)
	dst.Source = uefistore.SourceInstance(instanceID)
	key, err := uefistore.CopyObject(t.Context(), objects, testBucket, src, dst)
	require.NoError(t, err)

	keys, err := uefistore.ListObjects(t.Context(), objects, testBucket, imageID)
	require.NoError(t, err)
	assert.Equal(t, []string{key}, keys)
	got, _, err := uefistore.ReadObject(t.Context(), objects, testBucket, key, meta.Firmware.VarsSize)
	require.NoError(t, err)
	assert.Equal(t, region, got)
	_, _, err = uefistore.ReadObject(t.Context(), objects, testBucket, src, meta.Firmware.VarsSize)
	require.NoError(t, err, "the source stays")

	_, err = uefistore.CopyObject(t.Context(), objects, testBucket, uefistore.KeyPrefix(instanceID)+"gone", dst)
	require.ErrorIs(t, err, uefistore.ErrObjectNotFound)
}
