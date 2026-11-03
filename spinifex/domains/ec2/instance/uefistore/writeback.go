package uefistore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// ErrNotLive is a store record that is missing or deleting: its instance is
// gone or going, so no write-back is owed and nothing may be written under it.
var ErrNotLive = errors.New("uefistore: store record is not live")

// writeBackAttempts bounds the read-and-CAS loop that records or clears an
// obligation, so a record under constant contention fails rather than spins.
const writeBackAttempts = 3

// LiveWriteBack returns the write-back owed on instanceID's live record, or
// nil when none is. A missing or deleting record is ErrNotLive.
func (s *Store) LiveWriteBack(ctx context.Context, instanceID string) (*UEFIWriteBack, error) {
	rec, _, err := s.liveRecord(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	return rec.WriteBack, nil
}

// RecordWriteBack sets wb as the write-back owed on instanceID's live record,
// replacing any other: an instance has at most one open.
func (s *Store) RecordWriteBack(ctx context.Context, instanceID string, wb UEFIWriteBack) error {
	return s.mutateLive(ctx, instanceID, func(rec *UEFIStoreRecord) bool {
		rec.WriteBack = &wb
		return true
	})
}

// ClearWriteBack drops the write-back owed on instanceID's live record. With
// a key it drops only an obligation recorded for that key, so a retry does
// not clear one recorded after it read the record.
func (s *Store) ClearWriteBack(ctx context.Context, instanceID, key string) error {
	return s.mutateLive(ctx, instanceID, func(rec *UEFIStoreRecord) bool {
		if rec.WriteBack == nil || (key != "" && rec.WriteBack.Key != key) {
			return false
		}
		rec.WriteBack = nil
		return true
	})
}

// WriteBacksOwedBy returns the instances whose live record owes a write-back
// from node. A record that breaks its invariants is logged and passed over.
func (s *Store) WriteBacksOwedBy(ctx context.Context, node string) ([]string, error) {
	// Records are keyed by instance ID; the prefix passes over the bucket's
	// schema stamp, which shares the key space.
	recs, err := s.kv.List(ctx, "i-")
	if err != nil {
		return nil, err
	}
	var ids []string
	for i := range recs {
		rec := &recs[i]
		if err := rec.validate(rec.InstanceID); err != nil {
			slog.WarnContext(ctx, "Passing over a UEFI store record that cannot be read", "instanceId", rec.InstanceID, "err", err)
			continue
		}
		if rec.State == StateLive && rec.WriteBack != nil && rec.WriteBack.Node == node {
			ids = append(ids, rec.InstanceID)
		}
	}
	return ids, nil
}

// liveRecord reads instanceID's record, reporting a missing or deleting one
// as ErrNotLive.
func (s *Store) liveRecord(ctx context.Context, instanceID string) (*UEFIStoreRecord, uint64, error) {
	rec, rev, err := s.Get(ctx, instanceID)
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil, 0, fmt.Errorf("%w: %s has no store record", ErrNotLive, instanceID)
	}
	if err != nil {
		return nil, 0, err
	}
	if rec.State != StateLive {
		return nil, 0, fmt.Errorf("%w: %s is %s", ErrNotLive, instanceID, rec.State)
	}
	return rec, rev, nil
}

// mutateLive applies change to instanceID's live record under CAS, re-reading
// on a lost race. change reports whether it changed anything to write.
func (s *Store) mutateLive(ctx context.Context, instanceID string, change func(*UEFIStoreRecord) bool) error {
	for range writeBackAttempts {
		rec, rev, err := s.liveRecord(ctx, instanceID)
		if err != nil {
			return err
		}
		if !change(rec) {
			return nil
		}
		_, err = s.CompareAndSet(ctx, rec, rev)
		if errors.Is(err, kvstore.ErrConflict) {
			continue
		}
		return err
	}
	return fmt.Errorf("%w: %s changed under %d attempts", kvstore.ErrConflict, instanceID, writeBackAttempts)
}
