package uefistore

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
)

const (
	// SourceTemplate is a store seeded from the launching node's VARS template.
	SourceTemplate = "template"
	// SourceWriteBack is a store written back from a node's live copy.
	SourceWriteBack = "write-back"

	// PutTimeout bounds one put of a store object. The object is a fixed-size
	// region, so a put that takes longer is a wedged object store.
	PutTimeout = 10 * time.Second
)

// ErrInvalidObject is a store object whose key, region or metadata cannot be
// written as the owner's store.
var ErrInvalidObject = errors.New("uefistore: invalid store object")

// ObjectMeta is what a store object records about itself, as x-amz-meta-
// entries, so its owner, writer and firmware are known from the object alone.
type ObjectMeta struct {
	OwnerID    string
	WriterNode string
	Firmware   FirmwareIdentity
	CodePath   string
	Source     string
}

// Metadata returns m as object metadata, keyed by the owner's kind.
func (m ObjectMeta) Metadata() (map[string]*string, error) {
	var ownerKey string
	switch {
	case strings.HasPrefix(m.OwnerID, "i-"):
		ownerKey = "instance-id"
	case strings.HasPrefix(m.OwnerID, "ami-"):
		ownerKey = "image-id"
	default:
		return nil, fmt.Errorf("%w: owner %q is neither an instance nor an image", ErrInvalidObject, m.OwnerID)
	}
	switch {
	case m.WriterNode == "":
		return nil, fmt.Errorf("%w: %s names no writer node", ErrInvalidObject, m.OwnerID)
	case m.Source == "":
		return nil, fmt.Errorf("%w: %s names no source", ErrInvalidObject, m.OwnerID)
	case m.Firmware.Arch == "" || m.Firmware.VarsSize <= 0 || m.Firmware.CodeSHA256 == "":
		return nil, fmt.Errorf("%w: %s has an incomplete firmware identity", ErrInvalidObject, m.OwnerID)
	}
	return map[string]*string{
		ownerKey:               aws.String(m.OwnerID),
		"writer-node":          aws.String(m.WriterNode),
		"firmware-arch":        aws.String(m.Firmware.Arch),
		"firmware-vars-size":   aws.String(strconv.FormatInt(m.Firmware.VarsSize, 10)),
		"firmware-code-sha256": aws.String(m.Firmware.CodeSHA256),
		"firmware-code-path":   aws.String(m.CodePath),
		"source":               aws.String(m.Source),
	}, nil
}

// NewObjectKey returns a fresh key under ownerID's prefix, named by a random
// version 4 UUID, so no write ever replaces an object a record points at.
func NewObjectKey(ownerID string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate UEFI store key for %s: %w", ownerID, err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%s%x-%x-%x-%x-%x", KeyPrefix(ownerID), b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// PutObject writes region gzip-compressed to key with meta, within
// PutTimeout. The region must be exactly the firmware's VARS size, because
// pflash refuses any other.
func PutObject(ctx context.Context, objects objectstore.ObjectStore, bucket, key string, region []byte, meta ObjectMeta) error {
	if !ownsKey(meta.OwnerID, key) {
		return fmt.Errorf("%w: key %q is not under %s", ErrInvalidObject, key, KeyPrefix(meta.OwnerID))
	}
	if int64(len(region)) != meta.Firmware.VarsSize {
		return fmt.Errorf("%w: %s is a %d-byte region under firmware with a %d-byte VARS region",
			ErrInvalidObject, key, len(region), meta.Firmware.VarsSize)
	}
	md, err := meta.Metadata()
	if err != nil {
		return err
	}
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	if _, err := zw.Write(region); err != nil {
		return fmt.Errorf("compress UEFI store %s: %w", key, err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("compress UEFI store %s: %w", key, err)
	}

	putCtx, cancel := context.WithTimeout(ctx, PutTimeout)
	defer cancel()
	if _, err := objects.PutObject(putCtx, &s3.PutObjectInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		Body:     bytes.NewReader(body.Bytes()),
		Metadata: md,
	}); err != nil {
		return fmt.Errorf("put UEFI store %s: %w", key, err)
	}
	return nil
}
