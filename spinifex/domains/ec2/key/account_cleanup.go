package key

import (
	"context"
	"log/slog"
	"time"
)

// AccountCleanup is what one CleanupAccount run did. It holds key-pair IDs and
// counts only; it is the owner's account of its attempts, not proof of
// absence, which only a later ScanAccount gives.
type AccountCleanup struct {
	// DeletedKeyPairIDs completed the same owner delete DeleteKeyPair runs.
	DeletedKeyPairIDs []string
	// FailedKeyPairIDs had a store failure part way; the metadata remains.
	FailedKeyPairIDs []string
	// Referenced maps a kept key-pair ID to the reason the caller's predicate
	// gave for keeping it.
	Referenced map[string]string
	// Orphan public-key objects removed, that failed to delete, or left because
	// they were written after settledBefore and may belong to an in-flight create.
	OrphansRemoved, OrphansFailed, OrphansUnsettled int
	// OrphansReferenced counts public-key objects kept, by predicate reason.
	OrphansReferenced map[string]int
}

// CleanupAccount deletes every readable key pair of the account through the
// owner delete, and removes public-key objects no metadata names once they
// were last written before settledBefore. A key pair or public-key object
// for whose name referenced returns a reason is kept: IMDS serves the public
// key by name to any instance naming it. Unreadable metadata, and public keys that
// may belong to it, are left for the owner scan to report. A listing failure
// is returned as an error and nothing is deleted.
func (s *KeyServiceImpl) CleanupAccount(ctx context.Context, accountID string, settledBefore time.Time, referenced func(keyName string) string) (*AccountCleanup, error) {
	records, err := s.classifyAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if referenced == nil {
		referenced = func(string) string { return "" }
	}
	out := &AccountCleanup{Referenced: map[string]string{}, OrphansReferenced: map[string]int{}}
	for _, kp := range records.live {
		if reason := referenced(kp.name); reason != "" {
			out.Referenced[kp.id] = reason
			continue
		}
		if err := s.deleteKeyPairRecords(ctx, accountID, kp.name, kp.id); err != nil {
			out.FailedKeyPairIDs = append(out.FailedKeyPairIDs, kp.id)
			continue
		}
		out.DeletedKeyPairIDs = append(out.DeletedKeyPairIDs, kp.id)
	}
	for _, orphan := range records.orphans {
		if reason := referenced(orphan.name); reason != "" {
			out.OrphansReferenced[reason]++
			continue
		}
		if orphan.modified == nil || !orphan.modified.Before(settledBefore) {
			out.OrphansUnsettled++
			continue
		}
		if err := s.deleteObject(ctx, orphan.key); err != nil {
			slog.WarnContext(ctx, "Key-pair account cleanup: orphan public key delete failed", "err", err)
			out.OrphansFailed++
			continue
		}
		out.OrphansRemoved++
	}
	slog.InfoContext(ctx, "Key-pair account cleanup completed", "accountID", accountID,
		"deleted", len(out.DeletedKeyPairIDs), "failed", len(out.FailedKeyPairIDs), "referenced", len(out.Referenced),
		"orphansRemoved", out.OrphansRemoved, "orphansFailed", out.OrphansFailed,
		"orphansUnsettled", out.OrphansUnsettled)
	return out, nil
}
