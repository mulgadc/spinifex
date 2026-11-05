package zoneid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// BucketName holds one pinned record per Region.
const BucketName = "spinifex-zone-ids"

// Record is the pinned mapping for one Region.
type Record struct {
	ShortCode string            `json:"shortCode"`
	Zones     map[string]string `json:"zones"`
}

// Resolver assigns each zone name an ID once and serves the stored ID after.
type Resolver struct {
	store     *kvstore.Store[Record]
	region    string
	shortCode string
	key       string
	warnOnce  sync.Once
}

// NewResolver validates the override and returns a Resolver for region.
func NewResolver(js jetstream.JetStream, region, override string) (*Resolver, error) {
	code, err := ShortCodeFor(region, override)
	if err != nil {
		return nil, err
	}
	return &Resolver{
		store: kvstore.New[Record](js, kvstore.Config{
			Name:        BucketName,
			History:     1,
			Description: "Pinned availability-zone IDs per Region",
		}),
		region:    region,
		shortCode: code,
		key:       "zoneids." + region,
	}, nil
}

// Pin stores an ID for every name the record lacks, in sorted name order, and
// returns the stored ID for each name given. It is idempotent and safe under
// concurrent callers: the record is written by CAS and the first writer's
// assignment stands.
func (r *Resolver) Pin(ctx context.Context, azNames ...string) (map[string]string, error) {
	names := slices.Sorted(slices.Values(azNames))
	names = slices.Compact(names)
	var out map[string]string
	err := r.store.Upsert(ctx, r.key, func(rec *Record) (bool, error) {
		changed := false
		if rec.Zones == nil {
			rec.Zones = map[string]string{}
			rec.ShortCode = r.shortCode
			changed = true
		}
		if rec.ShortCode != r.shortCode {
			r.warnOnce.Do(func() {
				slog.Error("zone id short code differs from the pinned one; keeping the pinned code",
					"region", r.region, "configured", r.shortCode, "pinned", rec.ShortCode)
			})
		}
		for _, name := range names {
			if _, ok := rec.Zones[name]; ok {
				continue
			}
			rec.Zones[name] = rec.ShortCode + "-az" + strconv.Itoa(r.ordinalFor(rec, name))
			changed = true
		}
		out = make(map[string]string, len(names))
		for _, name := range names {
			out[name] = rec.Zones[name]
		}
		return changed, nil
	})
	if err != nil {
		return nil, fmt.Errorf("zoneid: pin %s: %w", r.region, err)
	}
	return out, nil
}

// ZoneID returns the stored ID for az, pinning it first when the record does
// not hold it, so an ID is never shown that was not stored.
func (r *Resolver) ZoneID(ctx context.Context, az string) (string, error) {
	rec, _, err := r.store.Get(ctx, r.key)
	switch {
	case err == nil:
		if id, ok := rec.Zones[az]; ok {
			return id, nil
		}
	case !errors.Is(err, kvstore.ErrNotFound):
		return "", fmt.Errorf("zoneid: read %s: %w", r.region, err)
	}
	ids, err := r.Pin(ctx, az)
	if err != nil {
		return "", err
	}
	return ids[az], nil
}

// ZoneName returns the zone name pinned to id, if any.
func (r *Resolver) ZoneName(ctx context.Context, id string) (string, bool, error) {
	rec, _, err := r.store.Get(ctx, r.key)
	if errors.Is(err, kvstore.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("zoneid: read %s: %w", r.region, err)
	}
	for name, v := range rec.Zones {
		if v == id {
			return name, true, nil
		}
	}
	return "", false, nil
}

// ordinalFor picks a zone's ordinal: its letter's position when the name is
// <region><letter> and that ordinal is free, otherwise the lowest free one.
func (r *Resolver) ordinalFor(rec *Record, az string) int {
	used := map[int]bool{}
	for _, id := range rec.Zones {
		if _, num, ok := strings.Cut(id, "-az"); ok {
			if n, err := strconv.Atoi(num); err == nil {
				used[n] = true
			}
		}
	}
	if rest, ok := strings.CutPrefix(az, r.region); ok && len(rest) == 1 && rest[0] >= 'a' && rest[0] <= 'z' {
		if n := int(rest[0]-'a') + 1; !used[n] {
			return n
		}
	}
	n := 1
	for used[n] {
		n++
	}
	return n
}

// Backfill pins the node's own zone first, then every other name in sorted
// order, so a zone that existed before the record did gets a stored ID before
// anything reads it. It runs at daemon start, off the request path.
func (r *Resolver) Backfill(ctx context.Context, ownAZ string, others []string) error {
	if ownAZ != "" {
		if _, err := r.Pin(ctx, ownAZ); err != nil {
			return err
		}
	}
	if len(others) == 0 {
		return nil
	}
	_, err := r.Pin(ctx, others...)
	return err
}
