// Package dirty is a read-only capability over the dirty-volume bucket: which
// node holds a volume's only current copy. The subjects, DTOs and bucket name
// it reads are owned by contracts/viperblockd/legacy/v1; this package has no
// vocabulary of its own and exposes no write, delete or purge operation.
//
// It is a leaf on purpose. A consumer of the contract -- an operator tool
// listing unsealed volumes -- should not have to link the storage adapter to
// read what it published.
package dirty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// UnsealedVolume reports a volume whose writes are not confirmed to have
// reached the backend, and which node holds them.
type UnsealedVolume struct {
	VolumeID string
	Owner    string
	Since    time.Time
	Reason   string
}

// bindDirtyBucket opens the dirty bucket read-only for an operator tool. It does
// not create the bucket: a cluster where no volume has ever been opened has no
// bucket, and reporting that as an error would read as a fault. A nil bucket
// with a nil error is that case.
//
// Unexported: this package offers only the read-only ListUnsealedVolumes
// capability, never a raw bucket handle a caller could write or purge through.
func bindDirtyBucket(ctx context.Context, nc *nats.Conn) (jetstream.KeyValue, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	kv, err := js.KeyValue(ctx, viperblocklegacyv1.DirtyBucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("volume dirty bucket: %w", err)
	}
	return kv, nil
}

// ListUnsealedVolumes reports every volume a node holds writes for that the
// backend may not have.
//
// Starting one elsewhere is allowed and preferred over an instance that cannot
// run, but it opens from the last checkpoint that did reach the backend. This
// is the list of volumes where that trade would cost something.
func ListUnsealedVolumes(ctx context.Context, nc *nats.Conn) ([]UnsealedVolume, error) {
	kv, err := bindDirtyBucket(ctx, nc)
	if err != nil || kv == nil {
		return nil, err
	}
	keys, err := kv.Keys(ctx)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list unsealed volumes: %w", err)
	}

	unsealed := make([]UnsealedVolume, 0, len(keys))
	for _, key := range keys {
		entry, err := kv.Get(ctx, key)
		if err != nil {
			continue
		}
		var record viperblocklegacyv1.DirtyRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			// Something holds writes for this volume even if the record cannot
			// be read, so report it rather than hide it.
			unsealed = append(unsealed, UnsealedVolume{VolumeID: key, Reason: "marker is unreadable"})
			continue
		}
		unsealed = append(unsealed, UnsealedVolume{
			VolumeID: key, Owner: record.Owner, Since: record.Since, Reason: record.Reason,
		})
	}
	return unsealed, nil
}
