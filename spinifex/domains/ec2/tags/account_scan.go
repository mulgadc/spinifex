package tags

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// ScanAccountResourceIDs returns the sorted IDs of the account's tagged
// resources that start with one of prefixes, from the same KV and legacy
// object listing DescribeTags reads. It reads keys only, never a tag value.
func (s *TagsServiceImpl) ScanAccountResourceIDs(ctx context.Context, accountID string, prefixes ...string) ([]string, error) {
	all, err := s.taggedResourceIDs(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, id := range all {
		for _, prefix := range prefixes {
			if strings.HasPrefix(id, prefix) {
				ids = append(ids, id)
				break
			}
		}
	}
	return ids, nil
}

// ScanTaggedResources returns, by account, the IDs of every tagged resource
// in the bucket that start with prefix. It reads keys only.
func (s *TagsServiceImpl) ScanTaggedResources(ctx context.Context, prefix string) (map[string][]string, error) {
	keys, err := kvstore.Keys(ctx, s.kv)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list tag keys: %w", err)
	}
	byAccount := map[string][]string{}
	for _, key := range keys {
		account, id, ok := strings.Cut(key, ".")
		if ok && account != "" && strings.HasPrefix(id, prefix) {
			byAccount[account] = append(byAccount[account], id)
		}
	}
	return byAccount, nil
}
