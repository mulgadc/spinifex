package uefistore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go/jetstream"
)

// markAttempts bounds MarkDeleting's retries against concurrent writers. Each
// lost race means another writer changed the record, and none of them can
// return it to live, so a few rounds always settle.
const markAttempts = 5

// Exists reports whether instanceID has a store record, without decoding it,
// so a record this node cannot read still counts as one.
func (s *Store) Exists(ctx context.Context, instanceID string) (bool, error) {
	return s.kv.Exists(ctx, instanceID)
}

// MarkDeleting records that instanceID's store is to be deleted. It reports
// false with no error when the instance has no store record. Only a decided
// terminate may call it.
func (s *Store) MarkDeleting(ctx context.Context, instanceID string) (bool, error) {
	for range markAttempts {
		rec, rev, err := s.Get(ctx, instanceID)
		if errors.Is(err, kvstore.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if rec.State == StateDeleting {
			return true, nil
		}
		rec.State = StateDeleting
		_, err = s.CompareAndSet(ctx, rec, rev)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, kvstore.ErrConflict) {
			return false, err
		}
	}
	return false, fmt.Errorf("uefistore: mark %s deleting: %w after %d attempts", instanceID, kvstore.ErrConflict, markAttempts)
}

// InstanceIDs lists the key of every store record, sorted. It decodes
// nothing, so a caller reads each record through Get and its validation.
func (s *Store) InstanceIDs(ctx context.Context) ([]string, error) {
	kv, err := s.kv.KV(ctx)
	if err != nil {
		return nil, err
	}
	lister, err := kv.ListKeys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("uefistore: list %s: %w", BucketName, err)
	}
	defer func() { _ = lister.Stop() }()

	var ids []string
	for key := range lister.Keys() {
		if key != kvstore.VersionKey {
			ids = append(ids, key)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// validOwner reports whether ownerID is an instance or image ID that names
// exactly one prefix, so a malformed ID can never widen a delete.
func validOwner(ownerID string) bool {
	if strings.ContainsAny(ownerID, "/*") {
		return false
	}
	for _, p := range []string{"i-", "ami-"} {
		if rest, ok := strings.CutPrefix(ownerID, p); ok && rest != "" {
			return true
		}
	}
	return false
}

// ListObjects returns the key of every object under ownerID's prefix, across
// every page of the listing.
func ListObjects(ctx context.Context, objects objectstore.ObjectStore, bucket, ownerID string) ([]string, error) {
	if !validOwner(ownerID) {
		return nil, fmt.Errorf("uefistore: %q is not an instance or image ID", ownerID)
	}
	contents, _, err := objectstore.ListAll(ctx, objects, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(KeyPrefix(ownerID)),
	})
	if err != nil {
		return nil, fmt.Errorf("uefistore: list %s: %w", KeyPrefix(ownerID), err)
	}
	keys := make([]string, 0, len(contents))
	for _, obj := range contents {
		keys = append(keys, aws.StringValue(obj.Key))
	}
	return keys, nil
}

// DeleteObjects deletes every object under ownerID's prefix and reports how
// many it removed. It succeeds only once a fresh listing finds the prefix
// empty, so an object written during the delete is not left unaccounted for.
func DeleteObjects(ctx context.Context, objects objectstore.ObjectStore, bucket, ownerID string) (int, error) {
	keys, err := ListObjects(ctx, objects, bucket, ownerID)
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, key := range keys {
		_, err := objects.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil && !objectstore.IsNoSuchKeyError(err) {
			return deleted, fmt.Errorf("uefistore: delete %s: %w", key, err)
		}
		deleted++
	}
	left, err := ListObjects(ctx, objects, bucket, ownerID)
	if err != nil {
		return deleted, err
	}
	if len(left) > 0 {
		return deleted, fmt.Errorf("uefistore: %d object(s) remain under %s", len(left), KeyPrefix(ownerID))
	}
	return deleted, nil
}
