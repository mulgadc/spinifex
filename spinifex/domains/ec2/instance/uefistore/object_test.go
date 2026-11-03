package uefistore_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"regexp"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBucket = "instances"

func testMeta(owner string) uefistore.ObjectMeta {
	return uefistore.ObjectMeta{
		OwnerID:    owner,
		WriterNode: "node-1",
		Firmware:   uefistore.FirmwareIdentity{Arch: "arm64", VarsSize: 1 << 16, CodeSHA256: "abc123"},
		CodePath:   "/usr/share/AAVMF/AAVMF_CODE.fd",
		Source:     uefistore.SourceTemplate,
	}
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Every key is a fresh version 4 UUID directly under the owner's prefix, so
// a write never lands on an object a record already points at.
func TestNewObjectKey_IsAFreshUUIDUnderTheOwnersPrefix(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		key, err := uefistore.NewObjectKey(instanceID)
		require.NoError(t, err)
		name, ok := bytes.CutPrefix([]byte(key), []byte(uefistore.KeyPrefix(instanceID)))
		require.True(t, ok, key)
		assert.Regexp(t, uuidV4, string(name))
		assert.False(t, seen[key], "key %s repeated", key)
		seen[key] = true
	}
}

func TestObjectMeta_Metadata_NamesTheOwnerByKind(t *testing.T) {
	md, err := testMeta(instanceID).Metadata()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"instance-id":          instanceID,
		"writer-node":          "node-1",
		"firmware-arch":        "arm64",
		"firmware-vars-size":   "65536",
		"firmware-code-sha256": "abc123",
		"firmware-code-path":   "/usr/share/AAVMF/AAVMF_CODE.fd",
		"source":               "template",
	}, aws.StringValueMap(md))

	img := testMeta("ami-0123456789abcdef0")
	img.Source = uefistore.SourceImage("ami-0fedcba9876543210")
	md, err = img.Metadata()
	require.NoError(t, err)
	assert.Equal(t, "ami-0123456789abcdef0", aws.StringValue(md["image-id"]))
	assert.NotContains(t, md, "instance-id")
	assert.Equal(t, "image:ami-0fedcba9876543210", aws.StringValue(md["source"]))
}

func TestObjectMeta_Metadata_RefusesAnIncompleteObject(t *testing.T) {
	tests := map[string]func(*uefistore.ObjectMeta){
		"unknown owner":  func(m *uefistore.ObjectMeta) { m.OwnerID = "vol-1" },
		"no writer":      func(m *uefistore.ObjectMeta) { m.WriterNode = "" },
		"no source":      func(m *uefistore.ObjectMeta) { m.Source = "" },
		"no arch":        func(m *uefistore.ObjectMeta) { m.Firmware.Arch = "" },
		"no vars size":   func(m *uefistore.ObjectMeta) { m.Firmware.VarsSize = 0 },
		"no code sha256": func(m *uefistore.ObjectMeta) { m.Firmware.CodeSHA256 = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			m := testMeta(instanceID)
			mutate(&m)
			_, err := m.Metadata()
			require.ErrorIs(t, err, uefistore.ErrInvalidObject)
		})
	}
}

// The 64 MiB arm64 region is mostly erased flash, so it is stored compressed
// and decompresses to the exact region.
func TestPutObject_StoresTheRegionCompressed(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	meta := testMeta(instanceID)
	region := bytes.Repeat([]byte{0xff}, int(meta.Firmware.VarsSize))
	copy(region, "BootOrder")
	key, err := uefistore.NewObjectKey(instanceID)
	require.NoError(t, err)

	require.NoError(t, uefistore.PutObject(t.Context(), objects, testBucket, key, region, meta))

	out, err := objects.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(testBucket), Key: aws.String(key)})
	require.NoError(t, err)
	body, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	assert.Less(t, len(body), len(region)/10, "the erased region compresses")
	gz, err := gzip.NewReader(bytes.NewReader(body))
	require.NoError(t, err)
	got, err := io.ReadAll(gz)
	require.NoError(t, err)
	assert.Equal(t, region, got)
}

// pflash needs a byte-exact region, and a key outside the owner's prefix is
// one no store record owns; either is refused before anything is put.
func TestPutObject_RefusesWhatTheOwnerCannotBoot(t *testing.T) {
	meta := testMeta(instanceID)
	good, err := uefistore.NewObjectKey(instanceID)
	require.NoError(t, err)
	other, err := uefistore.NewObjectKey("i-0fedcba9876543210")
	require.NoError(t, err)
	tests := []struct {
		name   string
		key    string
		region []byte
	}{
		{name: "short region", key: good, region: make([]byte, meta.Firmware.VarsSize-1)},
		{name: "long region", key: good, region: make([]byte, meta.Firmware.VarsSize+1)},
		{name: "another owner's key", key: other, region: make([]byte, meta.Firmware.VarsSize)},
		{name: "nested key", key: uefistore.KeyPrefix(instanceID) + "a/b", region: make([]byte, meta.Firmware.VarsSize)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := objectstore.NewMemoryObjectStore()
			err := uefistore.PutObject(t.Context(), objects, testBucket, tt.key, tt.region, meta)
			require.ErrorIs(t, err, uefistore.ErrInvalidObject)
			out, listErr := objects.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String(testBucket)})
			require.NoError(t, listErr)
			assert.Empty(t, out.Contents)
		})
	}
}

type failingPut struct {
	*objectstore.MemoryObjectStore
}

func (failingPut) PutObject(ctx context.Context, _ *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("put had no deadline")
	}
	return nil, errors.New("predastore unreachable")
}

// A put runs under a deadline, so a wedged object store fails the write
// rather than holding the caller, and its error reaches the caller.
func TestPutObject_IsBoundedAndReportsAFailedPut(t *testing.T) {
	key, err := uefistore.NewObjectKey(instanceID)
	require.NoError(t, err)
	meta := testMeta(instanceID)
	err = uefistore.PutObject(t.Context(), failingPut{objectstore.NewMemoryObjectStore()}, testBucket, key, make([]byte, meta.Firmware.VarsSize), meta)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "predastore unreachable")
}
