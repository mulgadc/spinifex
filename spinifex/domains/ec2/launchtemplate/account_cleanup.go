package launchtemplate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// AccountCleanup is what one CleanupAccountResidue run did. It holds template
// IDs and counts only; it is the owner's account of its attempts, not proof of
// absence, which only a later ScanAccount gives.
type AccountCleanup struct {
	// KeptTemplateIDs are live templates; account cleanup never deletes them.
	KeptTemplateIDs []string
	// Version bodies and name-index entries with no header: removed, failed,
	// or left because they were written after settledBefore and may belong to
	// a create still in flight.
	OrphanBodiesRemoved, OrphanBodiesFailed, OrphanBodiesUnsettled int
	OrphanNamesRemoved, OrphanNamesFailed, OrphanNamesUnsettled    int
}

// CleanupAccountResidue removes the account's version bodies and name-index
// entries whose header is absent, once written before settledBefore: without
// a header no ID, name or launch can resolve them. Live templates are listed,
// not deleted, because not every record that may name one is visible to EC2.
// Unreadable headers, their bodies and unrecognised entries are left for the
// owner scan to report. A listing failure is returned and nothing is removed.
func (s *LaunchTemplateServiceImpl) CleanupAccountResidue(ctx context.Context, accountID string, settledBefore time.Time) (*AccountCleanup, error) {
	keys, err := kvstore.Keys(ctx, s.kv)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list launch-template keys: %w", err)
	}

	prefix := accountID + "."
	present := make(map[string]bool)
	out := &AccountCleanup{}
	var bodies, names []string
	for _, key := range keys {
		rest, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		switch {
		case isHeaderKey(key, prefix):
			entry, err := s.kv.Get(ctx, key)
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			// An unreadable header still owns its bodies and name.
			present[rest] = true
			if err != nil {
				continue
			}
			var header LaunchTemplateHeader
			if json.Unmarshal(entry.Value(), &header) == nil && header.LaunchTemplateId == rest {
				out.KeptTemplateIDs = append(out.KeptTemplateIDs, rest)
			}
		case strings.HasPrefix(rest, "name."):
			names = append(names, key)
		case versionBodyTemplateID(rest) != "":
			bodies = append(bodies, key)
		}
	}

	for _, key := range bodies {
		if present[versionBodyTemplateID(strings.TrimPrefix(key, prefix))] {
			continue
		}
		tallyOrphan(s.removeSettled(ctx, key, settledBefore, nil), &out.OrphanBodiesRemoved, &out.OrphanBodiesFailed, &out.OrphanBodiesUnsettled)
	}
	for _, key := range names {
		// A name still naming a present header is that template's, not residue.
		owned := func(value []byte) bool { return present[string(value)] }
		tallyOrphan(s.removeSettled(ctx, key, settledBefore, owned), &out.OrphanNamesRemoved, &out.OrphanNamesFailed, &out.OrphanNamesUnsettled)
	}
	slog.InfoContext(ctx, "Launch-template account residue cleanup completed", "accountID", accountID,
		"kept", len(out.KeptTemplateIDs), "orphanBodiesRemoved", out.OrphanBodiesRemoved, "orphanNamesRemoved", out.OrphanNamesRemoved)
	return out, nil
}

// orphanOutcome is what removeSettled did with one headerless record.
type orphanOutcome int

const (
	orphanGone orphanOutcome = iota
	orphanRemoved
	orphanFailed
	orphanUnsettled
	orphanOwned
)

// removeSettled deletes key at the revision read when it was written before
// settledBefore and owned does not claim its value. The value is never decoded
// or logged: a version body holds template data and user data.
func (s *LaunchTemplateServiceImpl) removeSettled(ctx context.Context, key string, settledBefore time.Time, owned func([]byte) bool) orphanOutcome {
	entry, err := s.kv.Get(ctx, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return orphanGone
	case err != nil:
		slog.WarnContext(ctx, "Launch-template account cleanup: orphan read failed", "err", err)
		return orphanFailed
	case owned != nil && owned(entry.Value()):
		return orphanOwned
	case !entry.Created().Before(settledBefore):
		return orphanUnsettled
	}
	if err := s.kv.Delete(ctx, key, jetstream.LastRevision(entry.Revision())); err != nil {
		slog.WarnContext(ctx, "Launch-template account cleanup: orphan delete failed", "err", err)
		return orphanFailed
	}
	return orphanRemoved
}

func tallyOrphan(outcome orphanOutcome, removed, failed, unsettled *int) {
	switch outcome {
	case orphanRemoved:
		*removed++
	case orphanFailed:
		*failed++
	case orphanUnsettled:
		*unsettled++
	case orphanGone, orphanOwned:
	}
}

// AccountTemplateDeletes is what one DeleteLiveTemplates run did, by template
// ID. It is the owner's account of its attempts, not proof of absence.
type AccountTemplateDeletes struct {
	DeletedTemplateIDs []string
	FailedTemplateIDs  []string
	// Referenced are live templates left because referenced named them, with its reason.
	Referenced map[string]string
}

// DeleteLiveTemplates deletes the account's live templates through the same
// owner delete as DeleteLaunchTemplate, except those referenced names. A header
// it cannot read or decode is left for the owner scan to report. A listing
// failure is returned and nothing is deleted.
func (s *LaunchTemplateServiceImpl) DeleteLiveTemplates(ctx context.Context, accountID string, referenced func(id, name string) string) (*AccountTemplateDeletes, error) {
	keys, err := kvstore.Keys(ctx, s.kv)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list launch-template keys: %w", err)
	}
	prefix := accountID + "."
	out := &AccountTemplateDeletes{Referenced: map[string]string{}}
	for _, key := range keys {
		if !isHeaderKey(key, prefix) {
			continue
		}
		entry, err := s.kv.Get(ctx, key)
		if err != nil {
			continue
		}
		var header LaunchTemplateHeader
		if json.Unmarshal(entry.Value(), &header) != nil || prefix+header.LaunchTemplateId != key {
			continue
		}
		if reason := referenced(header.LaunchTemplateId, header.LaunchTemplateName); reason != "" {
			out.Referenced[header.LaunchTemplateId] = reason
			continue
		}
		if err := s.deleteTemplate(ctx, accountID, &header, entry); err != nil {
			slog.WarnContext(ctx, "Launch-template account cleanup: live template delete failed", "launchTemplateId", header.LaunchTemplateId, "err", err)
			out.FailedTemplateIDs = append(out.FailedTemplateIDs, header.LaunchTemplateId)
			continue
		}
		out.DeletedTemplateIDs = append(out.DeletedTemplateIDs, header.LaunchTemplateId)
	}
	slog.InfoContext(ctx, "Launch-template account live delete completed", "accountID", accountID,
		"deleted", len(out.DeletedTemplateIDs), "failed", len(out.FailedTemplateIDs), "referenced", len(out.Referenced))
	return out, nil
}
