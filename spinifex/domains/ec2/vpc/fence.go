package vpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrParentChanged reports a parent rewrite that lost its race: the record
// moved past the revision the caller read.
var ErrParentChanged = errors.New("vpc: parent record changed")

// Parents is the narrow capability another domain's admission fence needs on
// VPC and subnet records: read one at a revision, and rewrite it unchanged at
// that revision so a delete that checked the same revision loses.
type Parents struct {
	vpcKV    jetstream.KeyValue
	subnetKV jetstream.KeyValue
}

// NewParents wraps already-open VPC and subnet buckets.
func NewParents(vpcKV, subnetKV jetstream.KeyValue) *Parents {
	return &Parents{vpcKV: vpcKV, subnetKV: subnetKV}
}

// OpenParents opens the VPC and subnet buckets.
func OpenParents(ctx context.Context, js jetstream.JetStream) (*Parents, error) {
	vpcKV, err := kvstore.GetOrCreateBucket(ctx, js, KVBucketVPCs, 10)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", KVBucketVPCs, err)
	}
	subnetKV, err := kvstore.GetOrCreateBucket(ctx, js, KVBucketSubnets, 10)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", KVBucketSubnets, err)
	}
	return NewParents(vpcKV, subnetKV), nil
}

// VPC reads accountID's VPC and its revision; nil when absent.
func (p *Parents) VPC(ctx context.Context, accountID, vpcID string) (*VPCRecord, uint64, error) {
	return readParent[VPCRecord](ctx, p.vpcKV, kvstore.AccountKey(accountID, vpcID))
}

// Subnet reads accountID's subnet and its revision; nil when absent.
func (p *Parents) Subnet(ctx context.Context, accountID, subnetID string) (*SubnetRecord, uint64, error) {
	return readParent[SubnetRecord](ctx, p.subnetKV, kvstore.AccountKey(accountID, subnetID))
}

// TouchVPC rewrites the VPC's stored bytes unchanged at rev. A record that is
// absent or past rev is ErrParentChanged.
func (p *Parents) TouchVPC(ctx context.Context, accountID, vpcID string, rev uint64) error {
	return touchAt(ctx, p.vpcKV, kvstore.AccountKey(accountID, vpcID), rev)
}

// TouchSubnet is TouchVPC for a subnet.
func (p *Parents) TouchSubnet(ctx context.Context, accountID, subnetID string, rev uint64) error {
	return touchAt(ctx, p.subnetKV, kvstore.AccountKey(accountID, subnetID), rev)
}

func readParent[T any](ctx context.Context, kv jetstream.KeyValue, key string) (*T, uint64, error) {
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrInvalidKey) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("read %s: %w", key, err)
	}
	var v T
	if err := json.Unmarshal(entry.Value(), &v); err != nil {
		return nil, 0, fmt.Errorf("decode %s: %w", key, err)
	}
	return &v, entry.Revision(), nil
}

// touchAt rewrites the raw bytes, so a field this build does not know survives.
func touchAt(ctx context.Context, kv jetstream.KeyValue, key string, rev uint64) error {
	entry, err := kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrParentChanged
		}
		return fmt.Errorf("read %s: %w", key, err)
	}
	if entry.Revision() != rev {
		return ErrParentChanged
	}
	if _, err := kv.Update(ctx, key, entry.Value(), rev); err != nil {
		if kvstore.IsRevisionConflict(err) {
			return ErrParentChanged
		}
		return fmt.Errorf("rewrite %s: %w", key, err)
	}
	return nil
}

// markAttempts bounds how often a parent delete re-reads after its marked
// write loses a race.
const markAttempts = 5

// markDeleting rewrites the record at entry's revision with the deletion mark,
// keeping fields this build does not know, and returns the new revision.
func markDeleting(ctx context.Context, kv jetstream.KeyValue, key string, entry jetstream.KeyValueEntry) (uint64, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(entry.Value(), &raw); err != nil {
		return 0, fmt.Errorf("decode %s: %w", key, err)
	}
	raw["deleting"] = json.RawMessage("true")
	data, err := json.Marshal(raw)
	if err != nil {
		return 0, fmt.Errorf("encode %s: %w", key, err)
	}
	rev, err := kv.Update(ctx, key, data, entry.Revision())
	if err != nil {
		if kvstore.IsRevisionConflict(err) {
			return 0, ErrParentChanged
		}
		return 0, fmt.Errorf("mark %s: %w", key, err)
	}
	return rev, nil
}

// CountDeletionMarks counts VPC and subnet records that hold the deletion
// mark. An older daemon drops the mark when it rewrites a record, so rollback
// waits for this to reach zero. It never creates a bucket.
func CountDeletionMarks(ctx context.Context, js jetstream.JetStream) (int, error) {
	n := 0
	for _, name := range []string{KVBucketVPCs, KVBucketSubnets} {
		kv, err := js.KeyValue(ctx, name)
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("open %s: %w", name, err)
		}
		keys, err := kvstore.Keys(ctx, kv)
		if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
			return 0, fmt.Errorf("list %s: %w", name, err)
		}
		for _, key := range keys {
			if key == kvstore.VersionKey {
				continue
			}
			rec, _, err := readParent[struct {
				Deleting bool `json:"deleting"`
			}](ctx, kv, key)
			if err != nil {
				return 0, err
			}
			if rec != nil && rec.Deleting {
				n++
			}
		}
	}
	return n, nil
}
