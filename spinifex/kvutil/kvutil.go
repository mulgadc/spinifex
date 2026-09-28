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
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/mulgadc/bluebottle/pkg/safecast"
	"github.com/mulgadc/spinifex/spinifex/clustersize"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// GetOrCreateBucket creates or opens a KV bucket replicated across the cluster.
func GetOrCreateBucket(ctx context.Context, js jetstream.KeyValueManager, bucket string, history int) (jetstream.KeyValue, error) {
	return getOrCreateBucket(ctx, js, jetstream.KeyValueConfig{
		Bucket:  bucket,
		History: safecast.IntToUint8(history),
	})
}

// GetOrCreateBucketWithTTL is GetOrCreateBucket for buckets whose entries
// should age out on their own — request-dedupe records and other short-lived
// state that would otherwise accumulate without a sweeper. TTL applies at
// creation only; the replica count does not.
func GetOrCreateBucketWithTTL(ctx context.Context, js jetstream.KeyValueManager, bucket string, history int, ttl time.Duration) (jetstream.KeyValue, error) {
	return getOrCreateBucket(ctx, js, jetstream.KeyValueConfig{
		Bucket:  bucket,
		History: safecast.IntToUint8(history),
		TTL:     ttl,
	})
}

// BucketOptions is GetOrCreateBucket's full argument set, for callers needing a
// combination the named helpers do not cover. There is deliberately no Replicas
// field: see Replicas.
type BucketOptions struct {
	Name        string
	Description string
	History     int
	TTL         time.Duration
}

// GetOrCreateBucketWithOptions creates or opens a KV bucket from opts. Every
// field but Name applies at creation only: an existing bucket is opened with
// the config it already has, apart from its replica count.
func GetOrCreateBucketWithOptions(ctx context.Context, js jetstream.KeyValueManager, opts BucketOptions) (jetstream.KeyValue, error) {
	return getOrCreateBucket(ctx, js, jetstream.KeyValueConfig{
		Bucket:      opts.Name,
		Description: opts.Description,
		History:     safecast.IntToUint8(opts.History),
		TTL:         opts.TTL,
	})
}

func getOrCreateBucket(ctx context.Context, js jetstream.KeyValueManager, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	bucket := cfg.Bucket
	replicas, err := clustersize.Replicas()
	if err != nil {
		return nil, fmt.Errorf("create KV bucket %s: %w", bucket, err)
	}
	cfg.Replicas = replicas

	// Attach before create, so whether the bucket exists is this function's own
	// answer rather than an inference from which error the server reported
	// first. A create asking for more replicas than the cluster can place is
	// refused before the name is looked at, which would otherwise read as
	// "create failed" for a bucket that is present and serving.
	kv, err := js.KeyValue(ctx, bucket)
	switch {
	case err == nil:
		return openAndRaise(ctx, js, kv, bucket, replicas), nil
	case !errors.Is(err, jetstream.ErrBucketNotFound):
		return nil, fmt.Errorf("open KV bucket %s: %w", bucket, err)
	}

	kv, err = js.CreateKeyValue(ctx, cfg)
	if err == nil {
		return kv, nil
	}

	// A concurrent daemon created it in the gap, so opening it is the expected
	// outcome. Every other create failure is real and surfaces — a create at
	// fewer replicas than the cluster wants is the defect, so it must not be
	// reached by falling back.
	if !errors.Is(err, jetstream.ErrBucketExists) {
		return nil, fmt.Errorf("create KV bucket %s: %w", bucket, err)
	}
	kv, err = js.KeyValue(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("open KV bucket %s: %w", bucket, err)
	}
	return openAndRaise(ctx, js, kv, bucket, replicas), nil
}

// openAndRaise returns an already-open bucket, raising it to the cluster's
// replica count on a best-effort basis.
//
// An existing bucket may predate this rule, or predate the cluster growing, and
// raising it on open is what makes a formed cluster self-heal rather than wait
// for someone to run a repair. A failed raise is deliberately not a failed
// open: the config can legitimately name nodes that are not serving yet — that
// is what growing a cluster looks like — and refusing would stop every service
// on the host over a bucket that is present and quorate at the count it has.
// The daemon's sweep and `spx admin kv replicas --repair` finish the job.
func openAndRaise(ctx context.Context, js jetstream.KeyValueManager, kv jetstream.KeyValue, bucket string, replicas int) jetstream.KeyValue {
	if err := RaiseBucketReplicas(ctx, js, bucket, replicas); err != nil {
		slog.WarnContext(ctx, "Could not raise KV bucket to the cluster's replica count; it stays where it is until the cluster can hold more",
			"bucket", bucket, "want", replicas, "error", err)
	}
	return kv
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

// bucketListTimeout bounds one bucket listing. It is longer than the per-bucket
// Keys bound because this listing pages over every stream in the cluster rather
// than scanning one bucket, and it exists at all because a caller may hold a
// context that lives as long as the process. A var, not a const, so a test can
// shrink it.
var bucketListTimeout = 10 * time.Second

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
	listCtx, cancel := context.WithTimeout(ctx, bucketListTimeout)
	defer cancel()

	lister := js.KeyValueStoreNames(listCtx)

	// Selecting on the deadline rather than ranging the channel is what makes the
	// bound real: a lost stream-names reply leaves the channel open with nothing
	// ever sent on it, and a range parks here for the life of the process.
	// Cancelling listCtx on the way out unblocks the lister's send, so the early
	// return no longer leaks the goroutine it used to.
	var names []string
	for {
		select {
		case name, open := <-lister.Name():
			if !open {
				if err := lister.Error(); err != nil {
					return nil, fmt.Errorf("enumerate KV buckets: %w", err)
				}
				return names, nil
			}
			names = append(names, name)
		case <-listCtx.Done():
			return nil, fmt.Errorf("enumerate KV buckets: %w", listCtx.Err())
		}
	}
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
