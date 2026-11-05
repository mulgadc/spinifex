package image

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// uefiPointerAttempts bounds how often CreateImage re-reads a running
// instance's pointer after the object it named was pruned behind a newer one.
const uefiPointerAttempts = 3

// UEFIStoreRecords is the store-record read CreateImage makes of its source
// instance, answered by the stream leader so it sees the last landed pointer.
type UEFIStoreRecords interface {
	LeaderGet(ctx context.Context, instanceID string) (*uefistore.UEFIStoreRecord, uint64, error)
}

var _ UEFIStoreRecords = (*uefistore.Store)(nil)

// SetUEFIStores wires the store records CreateImage reads a UEFI instance's
// pointer from, and the node ID it writes image store objects as.
func (s *ImageServiceImpl) SetUEFIStores(records UEFIStoreRecords, nodeID string) {
	s.uefiRecords = records
	s.nodeID = nodeID
}

// copyInstanceUEFIStore copies the object instanceID's store record points at
// to a fresh key under imageID and returns its reference, or nil when the
// instance has no record or no pointer. The error is an AWS error code.
func (s *ImageServiceImpl) copyInstanceUEFIStore(ctx context.Context, instanceID, imageID string) (*ebsmetadata.UEFIDataRef, error) {
	if s.uefiRecords == nil || s.nodeID == "" {
		slog.ErrorContext(ctx, "CreateImage: no UEFI store records are wired on this node", "instanceId", instanceID)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	for range uefiPointerAttempts {
		rec, _, err := s.uefiRecords.LeaderGet(ctx, instanceID)
		switch {
		case errors.Is(err, kvstore.ErrNotFound):
			return nil, nil
		case err != nil:
			slog.ErrorContext(ctx, "CreateImage: failed to read the UEFI store record", "instanceId", instanceID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		case rec.State != uefistore.StateLive:
			return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
		case rec.Pointer == nil:
			return nil, nil
		}
		ptr := rec.Pointer
		key, err := uefistore.CopyObject(ctx, s.store, s.bucketName, ptr.Key, uefistore.ObjectMeta{
			OwnerID: imageID, WriterNode: s.nodeID, Firmware: ptr.Firmware, Source: uefistore.SourceInstance(instanceID),
		})
		if errors.Is(err, uefistore.ErrObjectNotFound) {
			// A write-back moved the pointer and a prune removed this one.
			continue
		}
		if err != nil {
			slog.ErrorContext(ctx, "CreateImage: failed to copy the UEFI variable store",
				"instanceId", instanceID, "imageId", imageID, "key", ptr.Key, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		slog.InfoContext(ctx, "CreateImage: copied the UEFI variable store",
			"instanceId", instanceID, "imageId", imageID, "sourceKey", ptr.Key, "key", key)
		return uefistore.AMIRef(key, ptr.Firmware), nil
	}
	slog.ErrorContext(ctx, "CreateImage: the UEFI store pointer kept naming a missing object",
		"instanceId", instanceID, "attempts", uefiPointerAttempts)
	return nil, errors.New(awserrors.ErrorServerInternal)
}

// copyImageUEFIStore copies the store src names to a fresh key under imageID,
// so deregistering either image leaves the other's intact. It returns nil
// when src names none.
func (s *ImageServiceImpl) copyImageUEFIStore(ctx context.Context, src ebsmetadata.AMI, imageID string) (*ebsmetadata.UEFIDataRef, error) {
	store, err := uefistore.ImageVarStoreFromAMI(src)
	if err != nil || store == nil {
		return nil, err
	}
	if s.nodeID == "" {
		return nil, errors.New("no node ID is wired to write UEFI store objects as")
	}
	key, err := uefistore.CopyObject(ctx, s.store, s.bucketName, store.Key, uefistore.ObjectMeta{
		OwnerID: imageID, WriterNode: s.nodeID, Firmware: store.Firmware, Source: uefistore.SourceImage(src.ImageID),
	})
	if err != nil {
		return nil, fmt.Errorf("copy UEFI store %s to %s: %w", store.Key, imageID, err)
	}
	return uefistore.AMIRef(key, store.Firmware), nil
}
