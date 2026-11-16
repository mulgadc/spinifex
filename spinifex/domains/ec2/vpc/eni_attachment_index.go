package vpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	"github.com/nats-io/nats.go/jetstream"
)

// The attachment index names, per instance, every interface that may be
// attached to it, under "{eniAccountID}.{instanceID}.{eniID}". The ENI record
// stays the truth: an entry only says which records to read.
const (
	KVBucketENIAttachments        = "spinifex-vpc-eni-attachments"
	KVBucketENIAttachmentsVersion = 1
)

// AttachmentIndexMinAge is how long an index entry must stand before a sweep may
// prune it. An attach writes its entry just before the ENI record, so a young
// entry whose record does not name the instance may be an attach in flight.
const AttachmentIndexMinAge = 5 * time.Minute

func init() {
	migrate.DefaultRegistry.RegisterKV(KVBucketENIAttachments, migrate.KVMigration{
		FromVersion: 0,
		ToVersion:   1,
		Description: "index the interfaces attached before the attachment index existed",
		Run: func(ctx context.Context, kvc migrate.KVContext) error {
			if kvc.JetStream == nil {
				return errors.New("attachment index backfill needs the ENI bucket")
			}
			eniKV, err := kvc.JetStream.KeyValue(ctx, KVBucketENIs)
			if err != nil {
				return fmt.Errorf("open %s: %w", KVBucketENIs, err)
			}
			_, err = indexAttachedENIs(ctx, eniKV, kvc.KV)
			return err
		},
	})
}

func eniAttachmentKey(accountID, instanceID, eniID string) string {
	return accountID + "." + instanceID + "." + eniID
}

// indexAttachment records that eniID may be attached to instanceID. Every attach
// writes it, even over an existing entry, so the entry's revision moves and a
// concurrent prune of the previous revision fails.
func (s *VPCServiceImpl) indexAttachment(ctx context.Context, accountID, instanceID, eniID string) error {
	if _, err := s.eniAttachKV.Put(ctx, eniAttachmentKey(accountID, instanceID, eniID), []byte(eniID)); err != nil {
		slog.ErrorContext(ctx, "ENI attachment index write failed", "eniId", eniID, "instanceId", instanceID, "err", err)
		return &storeError{err}
	}
	return nil
}

// InstanceAttachedENIs returns the interfaces in accountID attached to
// instanceID: those the attachment index names plus candidates, each read
// directly. A record that cannot be read or decoded fails the call.
func (s *VPCServiceImpl) InstanceAttachedENIs(ctx context.Context, accountID, instanceID string, candidates []string) ([]ENIRecord, error) {
	prefix := eniAttachmentKey(accountID, instanceID, "")
	lister, err := s.eniAttachKV.ListKeysFiltered(ctx, prefix+"*")
	if err != nil {
		return nil, fmt.Errorf("list attachment index for %s: %w", instanceID, err)
	}
	ids := slices.Clone(candidates)
	for key := range lister.Keys() {
		ids = append(ids, strings.TrimPrefix(key, prefix))
	}
	// Cancellation closes the lister early, and a short list must not read as detached.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)

	var out []ENIRecord
	for _, id := range ids {
		if id == "" {
			continue
		}
		entry, err := s.eniKV.Get(ctx, kvstore.AccountKey(accountID, id))
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read ENI %s: %w", id, err)
		}
		var record ENIRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			return nil, fmt.Errorf("decode ENI %s: %w", id, err)
		}
		if record.InstanceId == instanceID {
			out = append(out, record)
		}
	}
	return out, nil
}

// ReconcileAttachmentIndex indexes every attached interface the index lacks,
// which covers attaches written by a release without the index, and prunes
// entries older than minAge whose record no longer names their instance.
func (s *VPCServiceImpl) ReconcileAttachmentIndex(ctx context.Context, minAge time.Duration) (indexed, pruned int, err error) {
	indexed, err = indexAttachedENIs(ctx, s.eniKV, s.eniAttachKV)
	if err != nil {
		return indexed, 0, err
	}
	pruned, err = s.pruneAttachmentIndex(ctx, minAge)
	return indexed, pruned, err
}

// indexAttachedENIs creates the missing index entry of every attached record.
// An existing entry is left as it is, so its revision guards a prune as before.
func indexAttachedENIs(ctx context.Context, eniKV, indexKV jetstream.KeyValue) (int, error) {
	keys, err := eniKV.Keys(ctx)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", KVBucketENIs, err)
	}
	created := 0
	for _, key := range keys {
		if ctx.Err() != nil {
			return created, ctx.Err()
		}
		accountID, _, found := strings.Cut(key, ".")
		if key == kvstore.VersionKey || !found || accountID == "" {
			continue
		}
		entry, err := eniKV.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return created, fmt.Errorf("read %s: %w", key, err)
		}
		var record ENIRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			slog.WarnContext(ctx, "attachment index: undecodable ENI record skipped", "key", key, "err", err)
			continue
		}
		if record.InstanceId == "" || record.NetworkInterfaceId == "" {
			continue
		}
		indexKey := eniAttachmentKey(accountID, record.InstanceId, record.NetworkInterfaceId)
		_, err = indexKV.Create(ctx, indexKey, []byte(record.NetworkInterfaceId))
		switch {
		case err == nil:
			created++
		case errors.Is(err, jetstream.ErrKeyExists):
		default:
			return created, fmt.Errorf("index %s: %w", indexKey, err)
		}
	}
	return created, nil
}

// pruneAttachmentIndex deletes entries at least minAge old whose interface is
// gone or attached elsewhere. The delete is conditional on the revision read,
// so an attach that rewrote the entry in between keeps it.
func (s *VPCServiceImpl) pruneAttachmentIndex(ctx context.Context, minAge time.Duration) (int, error) {
	keys, err := s.eniAttachKV.Keys(ctx)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", KVBucketENIAttachments, err)
	}
	pruned := 0
	for _, key := range keys {
		if ctx.Err() != nil {
			return pruned, ctx.Err()
		}
		parts := strings.Split(key, ".")
		if key == kvstore.VersionKey || len(parts) != 3 {
			continue
		}
		accountID, instanceID, eniID := parts[0], parts[1], parts[2]
		entry, err := s.eniAttachKV.Get(ctx, key)
		if err != nil || time.Since(entry.Created()) < minAge {
			continue
		}
		attached, err := s.recordNamesInstance(ctx, accountID, eniID, instanceID)
		if err != nil || attached {
			continue
		}
		if err := s.eniAttachKV.Delete(ctx, key, jetstream.LastRevision(entry.Revision())); err != nil {
			continue
		}
		pruned++
	}
	return pruned, nil
}

// recordNamesInstance reports whether eniID's record is attached to instanceID; a
// missing record is not attached.
func (s *VPCServiceImpl) recordNamesInstance(ctx context.Context, accountID, eniID, instanceID string) (bool, error) {
	entry, err := s.eniKV.Get(ctx, kvstore.AccountKey(accountID, eniID))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var record ENIRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return false, err
	}
	return record.InstanceId == instanceID, nil
}
