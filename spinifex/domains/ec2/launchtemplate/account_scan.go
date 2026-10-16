package launchtemplate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// AccountScan is a read-only classification of every record under one
// account's prefix: headers, name index and version bodies. It holds template
// IDs and counts only; names, raw keys and record bodies never leave the scan.
type AccountScan struct {
	// LiveTemplateIDs are headers DescribeLaunchTemplates reports.
	LiveTemplateIDs []string
	// UnreadableTemplateIDs are headers found but not read or decoded.
	UnreadableTemplateIDs []string
	// OrphanVersionBodies are version bodies whose header key is absent.
	OrphanVersionBodies int
	// OrphanNameIndexEntries name a template whose header key is absent.
	OrphanNameIndexEntries int
	// UnreadableNameIndexEntries could not be read, so their target is unknown.
	UnreadableNameIndexEntries int
	// UnrecognisedEntries match none of the three record shapes.
	UnrecognisedEntries int
	// ReadFailures counts records the store failed to return.
	ReadFailures int
}

// ScanAccount reads the <accountID>.* records in the launch-template bucket.
// Headers and name-index entries are read; version bodies are classified from
// their keys alone, so template data and user data are never loaded.
func (s *LaunchTemplateServiceImpl) ScanAccount(ctx context.Context, accountID string) (*AccountScan, error) {
	keys, err := kvutil.Keys(ctx, s.kv)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list launch-template keys: %w", err)
	}

	prefix := accountID + "."
	headers := make(map[string]bool)
	var bodies, names []string
	scan := &AccountScan{}
	for _, key := range keys {
		rest, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		switch {
		case isHeaderKey(key, prefix):
			headers[rest] = s.scanHeader(ctx, key, rest, scan)
		case strings.HasPrefix(rest, "name."):
			names = append(names, key)
		case versionBodyTemplateID(rest) != "":
			bodies = append(bodies, versionBodyTemplateID(rest))
		default:
			scan.UnrecognisedEntries++
		}
	}

	for _, ltID := range bodies {
		if !headers[ltID] {
			scan.OrphanVersionBodies++
		}
	}
	for _, key := range names {
		entry, err := s.kv.Get(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
		case err != nil:
			slog.WarnContext(ctx, "Launch-template account scan: name index read failed", "err", err)
			scan.ReadFailures++
			scan.UnreadableNameIndexEntries++
		case !headers[string(entry.Value())]:
			scan.OrphanNameIndexEntries++
		}
	}
	return scan, nil
}

// scanHeader records one header as live or unreadable and reports whether it
// still exists. A decoder's message is discarded: it can quote stored bytes.
func (s *LaunchTemplateServiceImpl) scanHeader(ctx context.Context, key, ltID string, scan *AccountScan) bool {
	entry, err := s.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return false
		}
		slog.WarnContext(ctx, "Launch-template account scan: header read failed", "err", err)
		scan.ReadFailures++
		scan.UnreadableTemplateIDs = append(scan.UnreadableTemplateIDs, ltID)
		return true
	}
	var header LaunchTemplateHeader
	if err := json.Unmarshal(entry.Value(), &header); err != nil || header.LaunchTemplateId != ltID {
		scan.UnreadableTemplateIDs = append(scan.UnreadableTemplateIDs, ltID)
		return true
	}
	scan.LiveTemplateIDs = append(scan.LiveTemplateIDs, ltID)
	return true
}

// versionBodyTemplateID returns the template ID of a version-body key suffix
// (lt-<id>.v<n>), or "" for any other shape.
func versionBodyTemplateID(rest string) string {
	ltID, version, ok := strings.Cut(rest, ".v")
	if !ok || !strings.HasPrefix(ltID, "lt-") || strings.Contains(ltID, ".") {
		return ""
	}
	if n, err := strconv.ParseInt(version, 10, 64); err != nil || n < 1 {
		return ""
	}
	return ltID
}
