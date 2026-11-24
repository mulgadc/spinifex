package tgw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// SweepResult counts what one sweep pass changed.
type SweepResult struct {
	Moved       int // interrupted moves to the terminal bucket, finished
	Expired     int // terminal records past their visibility window
	KeysRemoved int // index keys and claims with no live owner
	TagsRemoved int // central tag entries of resources in neither bucket
}

// Sweep is the one pass that scans the buckets. It finishes interrupted
// moves, removes terminal records past their window, and removes residue
// older than the claim grace. It never removes a record that is not ended.
func (s *Store) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	if err := s.sweepLive(ctx, &res); err != nil {
		return res, err
	}
	if err := s.sweepTerminal(ctx, &res); err != nil {
		return res, err
	}
	if err := s.sweepTags(ctx, &res); err != nil {
		return res, fmt.Errorf("sweep central tags: %w", err)
	}
	return res, nil
}

func (s *Store) sweepLive(ctx context.Context, res *SweepResult) error {
	for _, b := range []struct {
		name string
		kv   jetstream.KeyValue
	}{{KVBucketTransitGateways, s.tgws}, {KVBucketAttachments, s.atts}} {
		keys, err := allKeys(ctx, b.kv)
		if err != nil {
			return fmt.Errorf("list %s: %w", b.name, err)
		}
		var rest []string
		for _, key := range keys {
			acct, id, ok := recordID(key)
			if !ok {
				rest = append(rest, key)
				continue
			}
			if err := s.finishInterrupted(ctx, b.kv, acct, id, res); err != nil {
				return fmt.Errorf("sweep %s: %w", key, err)
			}
		}
		for _, key := range rest {
			if err := s.sweepKey(ctx, b.kv, key, res); err != nil {
				return fmt.Errorf("sweep %s: %w", key, err)
			}
		}
	}
	return nil
}

// finishInterrupted completes a move whose terminal state is written in the
// live bucket.
func (s *Store) finishInterrupted(ctx context.Context, kv jetstream.KeyValue, accountID, id string, res *SweepResult) error {
	var err error
	if kv == s.atts {
		rec, rev, gerr := s.attachment(ctx, accountID, id)
		if gerr != nil || rec == nil || rec.isLive() {
			return gerr
		}
		err = s.finishAttachmentMove(ctx, rec, rev)
	} else {
		rec, rev, gerr := s.transitGateway(ctx, accountID, id)
		if gerr != nil || rec == nil || rec.State != StateDeleted {
			return gerr
		}
		err = s.finishTransitGatewayMove(ctx, rec, rev)
	}
	if errors.Is(err, errConflict) {
		return nil
	}
	if err == nil {
		res.Moved++
	}
	return err
}

// sweepKey removes an index key or claim older than the grace whose owner is
// not live.
func (s *Store) sweepKey(ctx context.Context, kv jetstream.KeyValue, key string, res *SweepResult) error {
	kind, _, _ := strings.Cut(key, ".")
	var owner, account string
	var rev uint64
	switch kind {
	case "acct", "tgw", "vpc", "subnet":
		entry, r, err := getJSON[indexEntry](ctx, kv, key)
		if err != nil || entry == nil || s.now().Sub(entry.CreatedAt) < claimGrace {
			return err
		}
		owner, account, rev = idFromIndexKey(key), entry.AccountID, r
	case "pair", "link", "slot":
		c, r, err := getJSON[claim](ctx, kv, key)
		if err != nil || c == nil || s.now().Sub(c.ClaimedAt) < claimGrace {
			return err
		}
		owner, account, rev = c.Owner, c.AccountID, r
	default:
		return nil
	}
	live, err := s.ownerLive(ctx, kv, account, owner)
	if err != nil || live {
		return err
	}
	if err := deleteAt(ctx, kv, key, rev); err != nil {
		if errors.Is(err, errConflict) {
			return nil
		}
		return err
	}
	res.KeysRemoved++
	slog.DebugContext(ctx, "Transit Gateway sweep removed an orphaned key", "key", key)
	return nil
}

// ownerLive keeps a key whose owner is live. A link. claim of a live holder is
// kept even when its record names another slot: the claim may precede the
// record write.
func (s *Store) ownerLive(ctx context.Context, kv jetstream.KeyValue, accountID, id string) (bool, error) {
	if kv == s.tgws {
		rec, _, err := s.transitGateway(ctx, accountID, id)
		return rec != nil && rec.State != StateDeleted, err
	}
	rec, _, err := s.attachment(ctx, accountID, id)
	return rec != nil && rec.isLive(), err
}

// sweepTerminal removes terminal records past their window, and account keys
// whose record is gone.
func (s *Store) sweepTerminal(ctx context.Context, res *SweepResult) error {
	keys, err := allKeys(ctx, s.term)
	if err != nil {
		return fmt.Errorf("list %s: %w", KVBucketTerminal, err)
	}
	var acctKeys []string
	for _, key := range keys {
		acct, id, ok := recordID(key)
		if !ok {
			if strings.HasPrefix(key, "acct.") {
				acctKeys = append(acctKeys, key)
			}
			continue
		}
		expired, rev, err := s.terminalExpired(ctx, key, id)
		if err != nil {
			return fmt.Errorf("sweep %s: %w", key, err)
		}
		if !expired {
			continue
		}
		if err := deleteAt(ctx, s.term, key, rev); err != nil && !errors.Is(err, errConflict) {
			return err
		}
		if err := deleteKey(ctx, s.term, acctKey(acct, id)); err != nil {
			return err
		}
		res.Expired++
	}
	for _, key := range acctKeys {
		entry, _, err := getJSON[indexEntry](ctx, s.term, key)
		if err != nil {
			return err
		}
		if entry == nil {
			continue
		}
		found, err := exists[struct{}](ctx, s.term, entry.AccountID, idFromIndexKey(key))
		if err != nil {
			return err
		}
		if found {
			continue
		}
		if err := deleteKey(ctx, s.term, key); err != nil {
			return err
		}
		res.KeysRemoved++
	}
	return nil
}

// terminalExpired reports a terminal record that is ended and past its window.
func (s *Store) terminalExpired(ctx context.Context, key, id string) (bool, uint64, error) {
	if isAttachmentID(id) {
		rec, rev, err := getJSON[AttachmentRecord](ctx, s.term, key)
		if err != nil || rec == nil || rec.isLive() || rec.TerminalAt.IsZero() {
			return false, 0, err
		}
		return !s.visible(rec.TerminalAt, attachmentWindow), rev, nil
	}
	rec, rev, err := getJSON[TransitGatewayRecord](ctx, s.term, key)
	if err != nil || rec == nil || rec.State != StateDeleted || rec.TerminalAt.IsZero() {
		return false, 0, err
	}
	return !s.visible(rec.TerminalAt, transitGatewayWindow), rev, nil
}

// SweepInterval is how often the cluster leader scans the buckets. The
// visibility windows are hours, so a record outlives its window by at most this.
const SweepInterval = 10 * time.Minute

// SweepReaper runs Sweep for the GC backstop at most once per SweepInterval.
type SweepReaper struct {
	store *Store

	mu   sync.Mutex
	last time.Time
}

// NewSweepReaper returns the sweep for the cluster-wide GC.
func (s *Store) NewSweepReaper() *SweepReaper { return &SweepReaper{store: s} }

func (r *SweepReaper) Class() string { return "transit-gateway-sweep" }

func (r *SweepReaper) Sweep(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.store.now()
	if !r.last.IsZero() && now.Sub(r.last) < SweepInterval {
		return 0, nil
	}
	res, err := r.store.Sweep(ctx)
	n := res.Moved + res.Expired + res.KeysRemoved + res.TagsRemoved
	if err != nil {
		return n, err
	}
	r.last = now
	if n > 0 {
		slog.InfoContext(ctx, "Transit Gateway sweep", "moved", res.Moved, "expired", res.Expired, "keys_removed", res.KeysRemoved, "tags_removed", res.TagsRemoved)
	}
	return n, nil
}
