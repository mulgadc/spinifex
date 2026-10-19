package placementgroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// BlockedGroup is a group the owner refused to delete because its member map
// still names instances. Only the count is kept; member IDs never leave.
type BlockedGroup struct {
	GroupID     string
	MemberCount int
}

// AccountCleanup is what one CleanupAccount run did, by group ID. It is the
// owner's account of its attempts, not proof of absence, which only a later
// ScanAccount gives.
type AccountCleanup struct {
	// DeletedGroupIDs were removed by the owner delete or were already gone.
	DeletedGroupIDs []string
	// FailedGroupIDs hit a store failure; the record remains.
	FailedGroupIDs []string
	// Blocked groups still hold instances and are never forced.
	Blocked []BlockedGroup
	// Referenced maps a kept group ID to the reason the caller's predicate gave.
	Referenced map[string]string
}

// CleanupAccount deletes every readable, empty placement group of the account
// through the same revision-guarded delete DeletePlacementGroup runs. A group
// holding instances is reported blocked, and one for whose ID or name
// referenced returns a reason is kept; unreadable records are left for the owner scan to report. A
// listing failure is returned and nothing is deleted.
func (s *PlacementGroupServiceImpl) CleanupAccount(ctx context.Context, accountID string, referenced func(groupID, groupName string) string) (*AccountCleanup, error) {
	keys, err := kvutil.Keys(ctx, s.kv)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list placement-group keys: %w", err)
	}

	prefix := accountID + "."
	out := &AccountCleanup{Referenced: map[string]string{}}
	for _, key := range keys {
		if key == kvutil.VersionKey || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, err := s.kv.Get(ctx, key)
		if err != nil {
			continue
		}
		var record PlacementGroupRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil || record.GroupId == "" {
			continue
		}
		if referenced != nil {
			reason := referenced(record.GroupId, record.GroupName)
			if reason == "" {
				reason = referenced(record.GroupId, strings.TrimPrefix(key, prefix))
			}
			if reason != "" {
				out.Referenced[record.GroupId] = reason
				continue
			}
		}
		members, err := s.deleteGroup(ctx, accountID, key)
		switch {
		case err == nil, errors.Is(err, errPGAbsent):
			out.DeletedGroupIDs = append(out.DeletedGroupIDs, record.GroupId)
		case errors.Is(err, errPGInUse):
			out.Blocked = append(out.Blocked, BlockedGroup{GroupID: record.GroupId, MemberCount: members})
		default:
			slog.WarnContext(ctx, "Placement-group account cleanup: delete failed", "groupId", record.GroupId, "err", err)
			out.FailedGroupIDs = append(out.FailedGroupIDs, record.GroupId)
		}
	}
	slog.InfoContext(ctx, "Placement-group account cleanup completed", "accountID", accountID,
		"deleted", len(out.DeletedGroupIDs), "failed", len(out.FailedGroupIDs), "blocked", len(out.Blocked), "referenced", len(out.Referenced))
	return out, nil
}
