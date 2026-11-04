// Package ebsobligation records the volume exports a detach left in place.
//
// A force detach, or a terminate whose unmount did not complete, clears a
// volume's attachment while the node that served it may still export it. The
// request cannot reach that node, so it records an UnpublishObligation here and
// returns; the named node settles it once it answers.
package ebsobligation

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/migrate"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Bucket holds one obligation per publish a cleared attachment left behind.
// It has no TTL: an obligation ends only when it is settled or closed.
const Bucket = "spinifex-ebs-unpublish-obligations"

// BucketVersion is the bucket schema version RunKV stamps.
const BucketVersion = 1

// SchemaVersion is the version of the UnpublishObligation record.
const SchemaVersion uint16 = 1

// UnpublishObligation names a publish a cleared attachment left on a node: the
// node, volume and handle, and the attachment it served. The attachment is what
// tells it apart from a later publish of the same volume on the same node.
type UnpublishObligation struct {
	SchemaVersion uint16    `json:"schema_version"`
	VolumeID      string    `json:"volume_id"`
	AccountID     string    `json:"account_id"`
	Handle        string    `json:"handle,omitempty"` // ProviderHandle when the attachment was cleared
	NodeID        string    `json:"node_id"`          // the node holding the publish
	InstanceID    string    `json:"instance_id"`      // the attachment that was cleared
	AttachedAt    time.Time `json:"attached_at"`
	RecordedAt    time.Time `json:"recorded_at"`
	Attempts      int       `json:"attempts"`
	Error         string    `json:"error,omitempty"`
}

// keyToken is one NATS subject token a key segment may be: the key is three
// tokens, so a segment holding a dot would split it and escape the filters.
var keyToken = regexp.MustCompile(`^[-_=a-zA-Z0-9]+$`)

// Key is the obligation's key, <node>.<volume-id>.<attached-at>, with the
// attachment time in Unix nanoseconds and an unknown time as 0.
func Key(o UnpublishObligation) (string, error) {
	if !keyToken.MatchString(o.NodeID) {
		return "", fmt.Errorf("unpublish obligation: node %q is not a valid key token", o.NodeID)
	}
	if !keyToken.MatchString(o.VolumeID) {
		return "", fmt.Errorf("unpublish obligation: volume %q is not a valid key token", o.VolumeID)
	}
	var nanos int64
	if !o.AttachedAt.IsZero() {
		nanos = o.AttachedAt.UnixNano()
	}
	return o.NodeID + "." + o.VolumeID + "." + strconv.FormatInt(nanos, 10), nil
}

// allKeys matches every obligation key and not the bucket's version stamp,
// which is a single token.
const allKeys = "*.*.*"

func bucketConfig() kvstore.Config {
	return kvstore.Config{
		Name:        Bucket,
		Description: "EBS exports a cleared attachment left on a node, until that node unpublishes them",
		History:     1,
		OnOpen: func(ctx context.Context, kv jetstream.KeyValue) error {
			return migrate.DefaultRegistry.RunKV(ctx, Bucket, kv, BucketVersion)
		},
		Missing: "EBS unpublish obligation bucket not initialized",
	}
}

// Store reads and writes the obligation bucket.
type Store struct {
	s *kvstore.Store[UnpublishObligation]
}

// NewStore returns a Store over js. The bucket is created on first use.
func NewStore(js jetstream.JetStream) *Store {
	return &Store{s: kvstore.New[UnpublishObligation](js, bucketConfig())}
}

// Open creates or opens the bucket now, so a node that cannot reach it fails
// at startup rather than on its first force detach.
func (s *Store) Open(ctx context.Context) error {
	_, err := s.s.KV(ctx)
	return err
}

// Record writes o if no obligation with its key exists. An existing key is the
// same obligation written by an earlier attempt, so it counts as written.
func (s *Store) Record(ctx context.Context, o UnpublishObligation) error {
	key, err := Key(o)
	if err != nil {
		return err
	}
	o.SchemaVersion = SchemaVersion
	if _, err := s.s.Create(ctx, key, &o); err != nil && !errors.Is(err, kvstore.ErrExists) {
		return fmt.Errorf("record unpublish obligation %s: %w", key, err)
	}
	return nil
}

// ListNode returns the open obligations naming node.
func (s *Store) ListNode(ctx context.Context, node string) ([]UnpublishObligation, error) {
	if !keyToken.MatchString(node) {
		return nil, fmt.Errorf("unpublish obligation: node %q is not a valid key token", node)
	}
	return s.list(ctx, node+".*.*")
}

// List returns every open obligation.
func (s *Store) List(ctx context.Context) ([]UnpublishObligation, error) {
	return s.list(ctx, allKeys)
}

func (s *Store) list(ctx context.Context, filter string) ([]UnpublishObligation, error) {
	items, _, err := s.s.Snapshot(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]UnpublishObligation, 0, len(items))
	for _, item := range items {
		out = append(out, item.Value)
	}
	return out, nil
}

// Close removes o. Removing one already gone is not an error.
func (s *Store) Close(ctx context.Context, o UnpublishObligation) error {
	key, err := Key(o)
	if err != nil {
		return err
	}
	return s.s.Delete(ctx, key)
}

// RecordFailure counts a failed settlement on o and keeps its error, so an
// operator listing the obligations sees why it is still open.
func (s *Store) RecordFailure(ctx context.Context, o UnpublishObligation, cause error) error {
	key, err := Key(o)
	if err != nil {
		return err
	}
	err = s.s.Mutate(ctx, key, func(cur *UnpublishObligation) (bool, error) {
		cur.Attempts++
		cur.Error = cause.Error()
		return true, nil
	})
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil
	}
	return err
}

// ListOpen reads the open obligations for an operator tool without creating
// the bucket: a cluster that has never force-detached a volume has none, and
// that is not an error.
func ListOpen(ctx context.Context, nc *nats.Conn) ([]UnpublishObligation, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	if _, err := js.KeyValue(ctx, Bucket); errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("unpublish obligation bucket: %w", err)
	}
	cfg := bucketConfig()
	cfg.AttachOnly = true
	cfg.OnOpen = nil
	return (&Store{s: kvstore.New[UnpublishObligation](js, cfg)}).List(ctx)
}
