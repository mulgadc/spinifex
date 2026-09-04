// Package vbwire is the contract between viperblockd and everything that has
// to read what it publishes: the NATS subjects it addresses, the shapes it puts
// on them, and the KV bucket recording which node holds a volume's only current
// copy.
//
// It is a leaf on purpose. A consumer of the contract -- the daemon stopping a
// fenced guest, an operator tool listing unsealed volumes -- should not have to
// link the storage service to name what it is listening for.
package vbwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// VolumeFencedSubject is where a fenced volume is announced so the daemon on
// this node can stop the guest that was using it. Node-addressed: the guest is
// local, and every other node's daemon has nothing to do about it.
func VolumeFencedSubject(node string) string {
	if node == "" {
		return "ebs.fenced"
	}
	return "ebs." + node + ".fenced"
}

// VolumeFencedEvent announces that a node tore down a volume's export because
// the lease moved. Winner is who holds it now, for the operator reading the
// guest's stop reason.
type VolumeFencedEvent struct {
	Volume string `json:"volume"`
	Node   string `json:"node"`
	Winner string `json:"winner"`
	Reason string `json:"reason"`
}

// DirtyBucket names the volumes whose writes are not confirmed to have reached
// the backend, so the node holding them is still known after that node is gone.
//
// Deliberately no TTL. The volume lease answers "who is writing this right now"
// and expires with its holder; this answers "whose copy is ahead of the
// backend", which has to outlive the holder to be worth anything.
//
// It is a placement input, not a gate. Instance start already prefers the node
// that last ran the instance and falls back after a window, and refusing the
// fallback here would trade a rare data-loss risk for a volume that cannot run
// anywhere.
const DirtyBucket = "VIPERBLOCK_VOLUME_DIRTY"

// DirtyRecord is what a node publishes about writes the backend may not have.
// Reason says how it got that way, so an operator reading a takeover warning
// does not have to correlate journals across nodes.
//
// Generation is the lease revision the writer held. It orders two nodes' claims
// on one volume, which is the whole reason a returning node cannot quietly
// overwrite or clear a marker that has moved on without it.
type DirtyRecord struct {
	Owner      string    `json:"owner"`
	Generation uint64    `json:"generation"`
	Since      time.Time `json:"since"`
	Reason     string    `json:"reason"`
}

// UnsealedVolume reports a volume whose writes are not confirmed to have
// reached the backend, and which node holds them.
type UnsealedVolume struct {
	VolumeID string
	Owner    string
	Since    time.Time
	Reason   string
}

// BindDirtyBucket opens the dirty bucket read-only for an operator tool. It does
// not create the bucket: a cluster where no volume has ever been opened has no
// bucket, and reporting that as an error would read as a fault. A nil bucket
// with a nil error is that case.
func BindDirtyBucket(ctx context.Context, nc *nats.Conn) (jetstream.KeyValue, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	kv, err := js.KeyValue(ctx, DirtyBucket)
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
	kv, err := BindDirtyBucket(ctx, nc)
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
		var record DirtyRecord
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
