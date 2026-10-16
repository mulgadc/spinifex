package placementgroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// ScannedGroup is one readable placement group: its ID and the instance IDs
// its member map still names, sorted.
type ScannedGroup struct {
	GroupID           string
	MemberInstanceIDs []string
}

// AccountScan is a read-only classification of every record under one
// account's prefix. Group names, tags and raw keys never leave the scan.
type AccountScan struct {
	Groups []ScannedGroup
	// Unreadable records were found but could not be read, decoded or
	// identified; their group IDs are unknown.
	Unreadable int
	// ReadFailures counts records the store failed to return.
	ReadFailures int
}

// ScanAccount reads every <accountID>.* record in the placement-group bucket.
// A listing failure is returned as an error; per-record read failures are
// counted. The bucket's schema-version key belongs to no account.
func (s *PlacementGroupServiceImpl) ScanAccount(ctx context.Context, accountID string) (*AccountScan, error) {
	keys, err := kvutil.Keys(ctx, s.kv)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list placement-group keys: %w", err)
	}

	prefix := accountID + "."
	scan := &AccountScan{}
	for _, key := range keys {
		if key == kvutil.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			slog.WarnContext(ctx, "Placement-group account scan: record read failed", "err", err)
			scan.ReadFailures++
			scan.Unreadable++
			continue
		}
		var record PlacementGroupRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil || record.GroupId == "" {
			scan.Unreadable++
			continue
		}
		scan.Groups = append(scan.Groups, ScannedGroup{GroupID: record.GroupId, MemberInstanceIDs: memberInstanceIDs(record.NodeInstances)})
	}
	return scan, nil
}

// memberInstanceIDs flattens a member map, skipping the empty placeholders a
// reservation leaves for a node, exactly as DeletePlacementGroup's in-use check does.
func memberInstanceIDs(nodeInstances map[string][]string) []string {
	var ids []string
	for _, onNode := range nodeInstances {
		for _, id := range onNode {
			if id != "" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	return ids
}
