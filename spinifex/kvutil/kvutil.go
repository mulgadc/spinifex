// Package kvutil holds the shared NATS KeyValue helpers: bucket get-or-create
// at the cluster's replica count, bucket enumeration, and schema-version
// stamping. It is built on github.com/nats-io/nats.go/jetstream, so every
// operation takes a context and honors the caller's deadline and cancellation
// rather than the legacy API's fixed internal wait.
//
// The helpers take a jetstream.KeyValueManager, which a jetstream.JetStream
// satisfies, so call sites pass their JetStream handle directly.
package kvutil

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mulgadc/bluebottle/pkg/safecast"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// GetOrCreateBucket creates or opens a KV bucket at the cluster's default
// replica count (see utils.SetDefaultKVReplicas), so buckets created lazily
// after boot are quorate on multi-node rather than stuck at R1.
func GetOrCreateBucket(ctx context.Context, js jetstream.KeyValueManager, bucket string, history int) (jetstream.KeyValue, error) {
	return GetOrCreateBucketWithReplicas(ctx, js, bucket, history, utils.DefaultKVReplicas())
}

// GetOrCreateBucketWithTTL is GetOrCreateBucket for buckets whose entries
// should age out on their own — request-dedupe records and other short-lived
// state that would otherwise accumulate without a sweeper. TTL applies at
// creation only, like Replicas.
func GetOrCreateBucketWithTTL(ctx context.Context, js jetstream.KeyValueManager, bucket string, history int, ttl time.Duration) (jetstream.KeyValue, error) {
	return getOrCreateBucket(ctx, js, jetstream.KeyValueConfig{
		Bucket:   bucket,
		History:  safecast.IntToUint8(history),
		Replicas: max(utils.DefaultKVReplicas(), 1),
		TTL:      ttl,
	})
}

// GetOrCreateBucketWithReplicas creates or opens a KV bucket at the given
// replica count (clamped to a minimum of 1). The replica count applies to
// creation only: an existing bucket is opened with the config it already has,
// so one created before the cluster grew is upgraded on rebalance, not here.
func GetOrCreateBucketWithReplicas(ctx context.Context, js jetstream.KeyValueManager, bucket string, history, replicas int) (jetstream.KeyValue, error) {
	return getOrCreateBucket(ctx, js, jetstream.KeyValueConfig{
		Bucket:   bucket,
		History:  safecast.IntToUint8(history),
		Replicas: max(replicas, 1),
	})
}

// BucketOptions is GetOrCreateBucket's full argument set, for callers needing a
// combination the three named helpers do not cover. A zero Replicas means the
// cluster default, matching those helpers.
type BucketOptions struct {
	Name        string
	Description string
	History     int
	Replicas    int
	TTL         time.Duration
}

// GetOrCreateBucketWithOptions creates or opens a KV bucket from opts. Like the
// named helpers, every field but Name applies at creation only: an existing
// bucket is opened with the config it already has.
func GetOrCreateBucketWithOptions(ctx context.Context, js jetstream.KeyValueManager, opts BucketOptions) (jetstream.KeyValue, error) {
	replicas := opts.Replicas
	if replicas == 0 {
		replicas = DefaultReplicas()
	}
	return getOrCreateBucket(ctx, js, jetstream.KeyValueConfig{
		Bucket:      opts.Name,
		Description: opts.Description,
		History:     safecast.IntToUint8(opts.History),
		Replicas:    max(replicas, 1),
		TTL:         opts.TTL,
	})
}

// DefaultReplicas is the cluster's default KV replica count, exposed so callers
// building a BucketOptions can tell "unset" from a deliberate 1.
func DefaultReplicas() int { return utils.DefaultKVReplicas() }

func getOrCreateBucket(ctx context.Context, js jetstream.KeyValueManager, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	bucket := cfg.Bucket
	kv, err := js.CreateKeyValue(ctx, cfg)
	if err == nil {
		return kv, nil
	}

	// A previous boot or a concurrent daemon already created it, so opening it
	// is the expected outcome. Every other create failure is real and surfaces.
	if !errors.Is(err, jetstream.ErrBucketExists) {
		return nil, fmt.Errorf("create KV bucket %s: %w", bucket, err)
	}
	kv, err = js.KeyValue(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("open KV bucket %s: %w", bucket, err)
	}
	return kv, nil
}

// IsStreamUnavailable reports whether err means the KV bucket's underlying
// JetStream stream was lost or is unreachable, which happens during NATS
// cluster formation when a low-replication stream is disrupted by a node
// joining or catching up. Each operation surfaces it differently:
//
//   - Get/Keys → ErrNoResponders ("no responders available for request")
//   - Put/Delete → ErrNoStreamResponse ("no response from stream")
//   - Direct stream queries → ErrStreamNotFound ("stream not found")
func IsStreamUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrNoStreamResponse) ||
		errors.Is(err, nats.ErrNoResponders) {
		return true
	}
	// Some paths wrap the condition as an untyped error, so the string is the
	// only signal left. Narrow, but dropping it would lose real detections.
	return strings.Contains(err.Error(), "stream not found")
}

// DeleteBucketIfExists deletes a KV bucket, treating an already-absent bucket
// as success so teardown paths are idempotent.
func DeleteBucketIfExists(ctx context.Context, js jetstream.KeyValueManager, bucket string) error {
	err := js.DeleteKeyValue(ctx, bucket)
	if err == nil || errors.Is(err, jetstream.ErrBucketNotFound) || errors.Is(err, jetstream.ErrStreamNotFound) {
		return nil
	}
	return fmt.Errorf("delete KV bucket %s: %w", bucket, err)
}

// BucketNames returns the name of every KV bucket, or an error if the listing
// could not be completed. The lister closes its channel both when the listing
// is complete and when the underlying stream-names request fails, so the
// terminal Error() check is the only thing separating a full listing from a
// truncated one. Callers that prune state for resources whose bucket is absent
// must therefore treat an error as "unknown", never as "no buckets" — a
// timeout would otherwise read as a fleet-wide deletion.
//
// The names are bucket names, already stripped of the KV_ stream prefix.
func BucketNames(ctx context.Context, js jetstream.KeyValueManager) ([]string, error) {
	lister := js.KeyValueStoreNames(ctx)

	// The lister goroutine blocks on an unbuffered send, so the channel is
	// drained fully — an early return here would leak it until ctx expires.
	var names []string
	for name := range lister.Name() {
		names = append(names, name)
	}
	if err := lister.Error(); err != nil {
		return nil, fmt.Errorf("enumerate KV buckets: %w", err)
	}
	return names, nil
}

// Keys lists a bucket's keys with the five-second bound used by the legacy API.
// The explicit timeout is required because the new Keys watcher otherwise uses
// a deadline-free Background context indefinitely.
func Keys(ctx context.Context, kv jetstream.KeyValue) ([]string, error) {
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	keys, err := kv.Keys(listCtx)
	if ctxErr := listCtx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return keys, err
}

// WriteVersion writes the schema version to a bucket, only if missing or older.
// Returns an error if the stored value is corrupt (non-integer).
func WriteVersion(ctx context.Context, kv jetstream.KeyValue, version int) error {
	entry, err := kv.Get(ctx, utils.VersionKey)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("read current version: %w", err)
	}
	if err == nil {
		stored, parseErr := strconv.Atoi(string(entry.Value()))
		if parseErr != nil {
			return fmt.Errorf("corrupted %s key (raw=%q): %w", utils.VersionKey, string(entry.Value()), parseErr)
		}
		if stored >= version {
			return nil
		}
	}
	_, err = kv.PutString(ctx, utils.VersionKey, strconv.Itoa(version))
	return err
}

// ReadVersion reads the schema version from a bucket; returns 0 if not set.
// Errors distinguish network failures and corrupt values from "not set".
func ReadVersion(ctx context.Context, kv jetstream.KeyValue) (int, error) {
	entry, err := kv.Get(ctx, utils.VersionKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("read version: %w", err)
	}
	v, parseErr := strconv.Atoi(string(entry.Value()))
	if parseErr != nil {
		return 0, fmt.Errorf("corrupted %s key (raw=%q): %w", utils.VersionKey, string(entry.Value()), parseErr)
	}
	return v, nil
}
