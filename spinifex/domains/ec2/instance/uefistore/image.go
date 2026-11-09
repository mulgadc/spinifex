package uefistore

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
)

const (
	// GetTimeout bounds one get of a store object, a fixed-size region.
	GetTimeout = 10 * time.Second

	// bodySlack is how far a compressed body may exceed its region before it
	// is refused; gzip grows incompressible data by far less.
	bodySlack = 1 << 20
)

// ErrObjectNotFound is a store object that does not exist.
var ErrObjectNotFound = errors.New("uefistore: store object does not exist")

// SourceImage is the source of a store copied from imageID's store.
func SourceImage(imageID string) string { return "image:" + imageID }

// SourceInstance is the source of an image store copied from instanceID's.
func SourceInstance(instanceID string) string { return "instance:" + instanceID }

// ImageVarStore is the store an AMI document names: one object under the
// image's own prefix and the firmware it was written under.
type ImageVarStore struct {
	ImageID  string
	Key      string
	Firmware FirmwareIdentity
}

// ImageVarStoreFromAMI returns the store ami names, or nil when it names none.
// A reference whose key is not one object under the image's own prefix, or
// that names no firmware, is ErrInvalidObject.
func ImageVarStoreFromAMI(ami ebsmetadata.AMI) (*ImageVarStore, error) {
	ref := ami.UEFIData
	if ref == nil {
		return nil, nil
	}
	if !strings.HasPrefix(ami.ImageID, "ami-") || !ownsKey(ami.ImageID, ref.Key) {
		return nil, fmt.Errorf("%w: %s names UEFI store %q outside %s", ErrInvalidObject, ami.ImageID, ref.Key, KeyPrefix(ami.ImageID))
	}
	if ref.Firmware.Arch == "" || ref.Firmware.VarsSize <= 0 {
		return nil, fmt.Errorf("%w: %s names UEFI store %q with no firmware arch and VARS size", ErrInvalidObject, ami.ImageID, ref.Key)
	}
	return &ImageVarStore{
		ImageID: ami.ImageID,
		Key:     ref.Key,
		Firmware: FirmwareIdentity{
			Arch: ref.Firmware.Arch, VarsSize: ref.Firmware.VarsSize, CodeSHA256: ref.Firmware.CodeSHA256,
		},
	}, nil
}

// AMIRef returns the reference an AMI document records for key under fw.
func AMIRef(key string, fw FirmwareIdentity) *ebsmetadata.UEFIDataRef {
	return &ebsmetadata.UEFIDataRef{
		Key:      key,
		Firmware: ebsmetadata.UEFIFirmware{Arch: fw.Arch, VarsSize: fw.VarsSize, CodeSHA256: fw.CodeSHA256},
	}
}

// ReadObject gets key within GetTimeout and returns its decompressed region,
// which must be exactly varsSize bytes, with the object's metadata. A missing
// object is ErrObjectNotFound and a malformed one ErrInvalidObject.
func ReadObject(ctx context.Context, objects objectstore.ObjectStore, bucket, key string, varsSize int64) ([]byte, map[string]*string, error) {
	getCtx, cancel := context.WithTimeout(ctx, GetTimeout)
	defer cancel()

	out, err := objects.GetObject(getCtx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, nil, fmt.Errorf("%w: %s", ErrObjectNotFound, key)
		}
		return nil, nil, fmt.Errorf("get UEFI store %s: %w", key, err)
	}
	defer out.Body.Close()

	limit := varsSize + bodySlack
	raw, err := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read UEFI store %s: %w", key, err)
	}
	if int64(len(raw)) > limit {
		return nil, nil, fmt.Errorf("%w: %s is over %d bytes compressed", ErrInvalidObject, key, limit)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s is not gzip: %w", ErrInvalidObject, key, err)
	}
	region, err := io.ReadAll(io.LimitReader(gz, varsSize+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s does not decompress: %w", ErrInvalidObject, key, err)
	}
	if int64(len(region)) != varsSize {
		return nil, nil, fmt.Errorf("%w: %s holds a %d-byte region, not the %d bytes its firmware needs",
			ErrInvalidObject, key, len(region), varsSize)
	}
	return region, out.Metadata, nil
}

// CopyObject reads srcKey, a region of meta.Firmware's VARS size, and puts it
// at a fresh key under meta.OwnerID, which it returns. A meta with no CodePath
// takes the source object's.
func CopyObject(ctx context.Context, objects objectstore.ObjectStore, bucket, srcKey string, meta ObjectMeta) (string, error) {
	region, srcMeta, err := ReadObject(ctx, objects, bucket, srcKey, meta.Firmware.VarsSize)
	if err != nil {
		return "", err
	}
	if meta.CodePath == "" {
		meta.CodePath = metadataValue(srcMeta, "firmware-code-path")
	}
	key, err := NewObjectKey(meta.OwnerID)
	if err != nil {
		return "", err
	}
	if err := PutObject(ctx, objects, bucket, key, region, meta); err != nil {
		return "", err
	}
	return key, nil
}

// metadataValue reads name from S3 object metadata, whose keys the SDK
// returns canonicalised.
func metadataValue(md map[string]*string, name string) string {
	for k, v := range md {
		if strings.EqualFold(k, name) {
			return aws.StringValue(v)
		}
	}
	return ""
}
