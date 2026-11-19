package vpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// CountPrefixListRules counts the security-group rules, in every account and
// direction, naming a prefix list. An absent bucket holds none.
func CountPrefixListRules(ctx context.Context, js jetstream.JetStream) (int, error) {
	kv, err := js.KeyValue(ctx, KVBucketSecurityGroups)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", KVBucketSecurityGroups, err)
	}
	keys, err := kvstore.Keys(ctx, kv)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", KVBucketSecurityGroups, err)
	}
	n := 0
	for _, key := range keys {
		if key == kvstore.VersionKey {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read security group %s: %w", key, err)
		}
		var rec SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return 0, fmt.Errorf("decode security group %s: %w", key, err)
		}
		for _, r := range rec.IngressRules {
			if r.PrefixListId != "" {
				n++
			}
		}
		for _, r := range rec.EgressRules {
			if r.PrefixListId != "" {
				n++
			}
		}
	}
	return n, nil
}
