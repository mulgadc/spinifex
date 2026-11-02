package kvstore

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// defaultReadTimeout bounds an enumerating read whose caller set no deadline:
// a bucket written faster than it drains keeps the reader catching up.
const defaultReadTimeout = 30 * time.Second

// readIdleRecheck is how long a read waits for its next delivery before asking
// the stream leader again, for a message that expired rather than being
// superseded and so will never be delivered.
const readIdleRecheck = 250 * time.Millisecond

// readRange reads every key matching filter and returns each key's latest
// entry, tombstones included, with the highest stream sequence delivered.
//
// The watcher's own "initial values done" marker is not trusted: it is
// computed from the consumer's pending count, which reads one short while a
// key in range is being rewritten, and the rewritten key is then missed. The
// read ends only when the stream leader's newest matching sequence is one it
// has already delivered, so every message in range at that moment was read or
// superseded by one that was.
func (s *Store[T]) readRange(ctx context.Context, kv jetstream.KeyValue, filter string, metaOnly bool) (map[string]jetstream.KeyValueEntry, uint64, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultReadTimeout)
		defer cancel()
	}

	var opts []jetstream.WatchOpt
	if metaOnly {
		opts = append(opts, jetstream.MetaOnly())
	}
	watcher, err := kv.Watch(ctx, filter, opts...)
	if err != nil {
		return nil, 0, fmt.Errorf("kvstore: read %s %s: %w", s.cfg.Name, filter, err)
	}
	defer func() { _ = watcher.Stop() }()

	latest := map[string]jetstream.KeyValueEntry{}
	var mark uint64
	target, err := s.lastInRange(ctx, kv, filter)
	if err != nil {
		return nil, 0, err
	}

	idle := time.NewTimer(readIdleRecheck)
	defer idle.Stop()
	for {
		if mark >= target {
			if target, err = s.lastInRange(ctx, kv, filter); err != nil {
				return nil, 0, err
			}
			if mark >= target {
				return latest, mark, nil
			}
		}
		idle.Reset(readIdleRecheck)
		select {
		case <-ctx.Done():
			return nil, 0, fmt.Errorf("kvstore: read %s %s: %w", s.cfg.Name, filter, ctx.Err())
		case <-idle.C:
			if target, err = s.lastInRange(ctx, kv, filter); err != nil {
				return nil, 0, err
			}
			if mark >= target {
				return latest, mark, nil
			}
		case entry, ok := <-watcher.Updates():
			if !ok {
				return nil, 0, fmt.Errorf("kvstore: read %s %s: watcher closed early", s.cfg.Name, filter)
			}
			// nil marks the watcher's own end of the initial values, which
			// is the count this read does not trust.
			if entry == nil {
				continue
			}
			if prev, seen := latest[entry.Key()]; !seen || entry.Revision() > prev.Revision() {
				latest[entry.Key()] = entry
			}
			mark = max(mark, entry.Revision())
		}
	}
}

// lastInRange is the stream leader's newest sequence matching filter. A store
// built without a JetStream client can still answer for the whole bucket from
// the bucket's own stream info; a narrower range it cannot verify is an error.
func (s *Store[T]) lastInRange(ctx context.Context, kv jetstream.KeyValue, filter string) (uint64, error) {
	s.mu.Lock()
	js := s.js
	s.mu.Unlock()
	if js != nil {
		return s.LastSequence(ctx, filter)
	}
	if filter != ">" {
		return 0, fmt.Errorf("kvstore: read %s %s: no JetStream client to verify a filtered read", s.cfg.Name, filter)
	}
	status, err := kv.Status(ctx)
	if err != nil {
		return 0, fmt.Errorf("kvstore: read %s: status: %w", s.cfg.Name, err)
	}
	info, ok := status.(*jetstream.KeyValueBucketStatus)
	if !ok {
		return 0, fmt.Errorf("kvstore: read %s: status carries no stream info", s.cfg.Name)
	}
	return info.StreamInfo().State.LastSeq, nil
}

// live reports whether an entry is a value rather than a delete or purge.
func live(entry jetstream.KeyValueEntry) bool {
	return entry.Operation() == jetstream.KeyValuePut
}
