package tags

import (
	"context"
	"strings"
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
