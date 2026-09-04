package viperblockd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// volumeLeaseBucket holds one entry per volume that some node has a viperblock
// engine open on. JetStream serialises the create, so the entry is the
// exclusion rather than a record of it.
const volumeLeaseBucket = "VIPERBLOCK_VOLUME_LEASES"

// volumeLeaseTTL is how long a lease outlives the node holding it. A node that
// dies mid-mount leaves its entry behind and nothing else may open the volume
// until the entry ages out.
const volumeLeaseTTL = 45 * time.Second

// volumeLeaseRenewInterval keeps a live holder's entry young. Well inside the
// TTL so one missed renewal does not surrender the lease.
const volumeLeaseRenewInterval = 15 * time.Second

// volumeLeaseRenewTimeout bounds one renewal attempt. Without it a JetStream
// call that hangs holds the renewal goroutine past the point where the server
// has already given the entry away, and the holder never notices.
const volumeLeaseRenewTimeout = 5 * time.Second

// volumeLeaseValidity is how long a holder may keep writing after its last
// *confirmed* renewal. Set below volumeLeaseTTL on purpose: a server-side TTL
// is not a lease unless the holder stops before the server may re-grant, so the
// difference is the margin covering scheduling delay and request latency.
const volumeLeaseValidity = 30 * time.Second

// volumeLeaseCheckInterval is how often validity is tested. Shorter than the
// renewal interval so a lapsed holder is fenced on its own schedule rather than
// on the next renewal that happens to come due.
const volumeLeaseCheckInterval = 5 * time.Second

// errVolumeLeaseHeld reports that another claimant owns the volume. It is the
// one lease failure a caller can do something about, so it is distinguishable.
var errVolumeLeaseHeld = errors.New("volume is leased by another owner")

// errNoVolumeLeaseStore reports that exclusive access could not be established
// at all. Distinguishable from a refusal so a caller can tell "somebody else
// holds this" from "nobody can say who does".
var errNoVolumeLeaseStore = errors.New("no volume lease store")

// volumeLeaseKeyPattern is what JetStream KV accepts as a key. Volume names
// reach here from the wire, and a name carrying "." or ">" would address
// somebody else's key.
var volumeLeaseKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// volumeLeaseRecord is what a holder publishes about itself. Generation is the
// revision that won the race: monotonic across the bucket, so a later opener
// always carries a higher one than the writer it displaced.
type volumeLeaseRecord struct {
	Owner      string    `json:"owner"`
	Generation uint64    `json:"generation"`
	AcquiredAt time.Time `json:"acquired_at"`
}

// leaseLossKind is why a holder stopped being able to prove it owns a volume.
// The two are not the same event and a fence reports them separately: one says
// the cluster moved the volume, the other says this node lost contact with it.
type leaseLossKind int

const (
	// leaseLostToPeer: a conditional update was rejected, so the entry changed
	// under this node and another may hold it now.
	leaseLostToPeer leaseLossKind = iota

	// leaseLostStalled: renewal went unconfirmed for long enough that the server
	// TTL is about to admit another owner. Nobody has necessarily taken it yet.
	leaseLostStalled
)

// volumeLeases hands out cluster-wide volume leases from a JetStream KV
// bucket, and remembers which ones this node holds.
type volumeLeases struct {
	kv    jetstream.KeyValue
	owner string

	// onLost is called when this node can no longer prove it owns a volume. Nil
	// leaves the loss logged and nothing else, which is what a unit test with no
	// export to tear down wants.
	onLost func(context.Context, string, leaseLossKind)

	mu   sync.Mutex
	held map[string]*volumeLease
}

// newVolumeLeases binds the lease bucket, creating it if this is the first
// node up. owner identifies this node in the entries it writes.
func newVolumeLeases(ctx context.Context, nc *nats.Conn, owner string) (*volumeLeases, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      volumeLeaseBucket,
		Description: "one entry per volume with a viperblock engine open on it",
		TTL:         volumeLeaseTTL,
		History:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("volume lease bucket: %w", err)
	}
	return &volumeLeases{kv: kv, owner: owner, held: make(map[string]*volumeLease)}, nil
}

// volumeLease is one held lease and the goroutine keeping it alive.
type volumeLease struct {
	leases     *volumeLeases
	key        string
	volume     string
	generation uint64

	stop context.CancelFunc
	done chan struct{}

	mu sync.Mutex
	// revision is the entry version this holder last wrote, and is what every
	// subsequent write is conditioned on.
	revision uint64
	// lost records that the entry moved out from under this holder, which
	// means somebody else may now be writing the volume.
	lost bool
	// confirmed is when the last conditional update was acknowledged by the
	// server. An attempt still in flight does not count: only a write the
	// server accepted proves the entry is still ours.
	confirmed time.Time
	// refs counts opens on this node sharing the lease. The lease is released
	// when the last one lets go.
	refs int
}

// acquire claims volumeName for this node, or reports who has it. Repeat
// acquisitions on this node share one lease: cross-node exclusion is what the
// lease is for, and the per-node case is already held by the volume flock.
func (l *volumeLeases) acquire(ctx context.Context, volumeName string) (*volumeLease, error) {
	if !volumeLeaseKeyPattern.MatchString(volumeName) {
		return nil, fmt.Errorf("volume name %q cannot be a lease key", volumeName)
	}

	l.mu.Lock()
	if lease, ok := l.held[volumeName]; ok {
		lease.mu.Lock()
		lease.refs++
		lease.mu.Unlock()
		l.mu.Unlock()
		return lease, nil
	}
	l.mu.Unlock()

	record := volumeLeaseRecord{Owner: l.owner, AcquiredAt: time.Now().UTC()}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("marshal lease: %w", err)
	}

	revision, err := l.kv.Create(ctx, volumeName, payload)
	switch {
	case errors.Is(err, jetstream.ErrKeyExists):
		if revision, err = l.takeOver(ctx, volumeName, payload); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("claim lease for %s: %w", volumeName, err)
	}

	lease := &volumeLease{
		leases:     l,
		key:        volumeName,
		volume:     volumeName,
		generation: revision,
		revision:   revision,
		confirmed:  time.Now(),
		done:       make(chan struct{}),
		refs:       1,
	}

	// The generation is the revision the create returned, so it can only be
	// published afterwards. A failure here costs observability, not exclusion.
	record.Generation = revision
	if published, perr := json.Marshal(record); perr == nil {
		if rev, uerr := l.kv.Update(ctx, volumeName, published, revision); uerr == nil {
			lease.revision = rev
		} else {
			slog.Warn("volume lease: could not publish generation", "volume", volumeName, "err", uerr)
		}
	}

	l.mu.Lock()
	l.held[volumeName] = lease
	l.mu.Unlock()

	renewCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	lease.stop = cancel
	go lease.renewLoop(renewCtx)

	slog.Info("volume lease acquired", "volume", volumeName, "owner", l.owner, "generation", revision)
	return lease, nil
}

// takeOver reclaims an entry this node left behind. A daemon restart abandons
// the leases of exports that outlived it, and those exports are still running:
// waiting out the TTL would leave them untracked, and the entry cannot belong
// to a live remote holder because it names this node.
//
// The write is conditioned on the revision just read, so two claimants racing
// to reclaim the same entry cannot both win.
func (l *volumeLeases) takeOver(ctx context.Context, volumeName string, payload []byte) (uint64, error) {
	entry, err := l.kv.Get(ctx, volumeName)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", errVolumeLeaseHeld, l.describeHolder(ctx, volumeName))
	}

	var record volumeLeaseRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil || record.Owner != l.owner {
		return 0, fmt.Errorf("%w: %s", errVolumeLeaseHeld, l.describeHolder(ctx, volumeName))
	}

	revision, err := l.kv.Update(ctx, volumeName, payload, entry.Revision())
	if err != nil {
		return 0, fmt.Errorf("%w: %s", errVolumeLeaseHeld, l.describeHolder(ctx, volumeName))
	}
	slog.Info("volume lease reclaimed from this node's previous run", "volume", volumeName, "owner", l.owner)
	return revision, nil
}

// describeHolder reports who owns volumeName, for the error the loser gets.
// Best effort: the point of the message is triage, not control flow.
func (l *volumeLeases) describeHolder(ctx context.Context, volumeName string) string {
	entry, err := l.kv.Get(ctx, volumeName)
	if err != nil {
		return "holder unknown"
	}
	var record volumeLeaseRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return "holder unreadable"
	}
	return fmt.Sprintf("held by %s since %s", record.Owner, record.AcquiredAt.Format(time.RFC3339))
}

// release drops one reference, and gives the lease up once the last one goes.
func (l *volumeLeases) release(ctx context.Context, lease *volumeLease) {
	if lease == nil {
		return
	}

	lease.mu.Lock()
	lease.refs--
	remaining := lease.refs
	lease.mu.Unlock()
	if remaining > 0 {
		return
	}

	l.mu.Lock()
	delete(l.held, lease.key)
	l.mu.Unlock()

	lease.stop()
	<-lease.done

	// A lost lease belongs to somebody else now. Deleting the key would evict
	// the live holder and hand the volume to a third opener.
	lease.mu.Lock()
	lost, revision := lease.lost, lease.revision
	lease.mu.Unlock()
	if lost {
		slog.Warn("volume lease released without deleting: entry no longer ours", "volume", lease.volume)
		return
	}

	if err := l.kv.Delete(ctx, lease.key, jetstream.LastRevision(revision)); err != nil {
		slog.Warn("volume lease: delete failed, entry will expire", "volume", lease.volume, "ttl_ms", otelsetup.Millis(volumeLeaseTTL), "err", err)
		return
	}
	slog.Info("volume lease released", "volume", lease.volume, "generation", lease.generation)
}

// renewLoop refreshes the entry so the bucket TTL does not expire a live
// holder, notices when the lease has been taken away, and — the part that makes
// it a lease rather than a hint — gives the volume up when it can no longer
// prove the entry is still ours.
//
// The validity check is separate from the renewal because the two answer
// different questions. Renewal asks "can I still reach JetStream"; validity
// asks "may I still write". A holder partitioned from NATS keeps failing the
// first indefinitely while the server hands the entry to somebody else, and
// only the second stops it.
func (lease *volumeLease) renewLoop(ctx context.Context) {
	defer close(lease.done)

	ticker := time.NewTicker(volumeLeaseCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if lease.expiredLocally() {
				lease.surrender(ctx)
				return
			}
			if time.Since(lease.lastConfirmed()) < volumeLeaseRenewInterval {
				continue
			}
			if !lease.renew(ctx) {
				return
			}
		}
	}
}

// expiredLocally reports that this holder has gone longer than
// volumeLeaseValidity without a confirmed renewal, so the server may be about
// to grant the entry to somebody else.
func (lease *volumeLease) expiredLocally() bool {
	return time.Since(lease.lastConfirmed()) >= volumeLeaseValidity
}

func (lease *volumeLease) lastConfirmed() time.Time {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.confirmed
}

// surrender gives the volume up because this holder can no longer prove it owns
// it. Marked lost so the release path cannot delete an entry that may already
// belong to another node.
func (lease *volumeLease) surrender(ctx context.Context) {
	lease.mu.Lock()
	lease.lost = true
	since := time.Since(lease.confirmed)
	lease.mu.Unlock()

	slog.Error("volume lease could not be confirmed before its TTL, surrendering the volume",
		"volume", lease.volume, "generation", lease.generation,
		"unconfirmed_for_ms", otelsetup.Millis(since), "ttl_ms", otelsetup.Millis(volumeLeaseTTL))

	if onLost := lease.leases.onLost; onLost != nil {
		go onLost(context.WithoutCancel(ctx), lease.volume, leaseLostStalled)
	}
}

// renew rewrites the entry conditioned on the revision this holder last saw,
// and reports whether the lease is still ours.
//
// Losing it hands off to onLost, which fences the export. That runs on its own
// goroutine because fencing releases the lease, and releasing waits for this
// one to exit.
func (lease *volumeLease) renew(ctx context.Context) bool {
	record := volumeLeaseRecord{Owner: lease.leases.owner, Generation: lease.generation, AcquiredAt: time.Now().UTC()}
	payload, err := json.Marshal(record)
	if err != nil {
		slog.Error("volume lease: marshal renewal", "volume", lease.volume, "err", err)
		return true
	}

	lease.mu.Lock()
	revision := lease.revision
	lease.mu.Unlock()

	// Bounded so a hung JetStream call cannot hold this goroutine past the
	// point where the server has already re-granted the entry.
	ctx, cancel := context.WithTimeout(ctx, volumeLeaseRenewTimeout)
	defer cancel()

	renewed, err := lease.leases.kv.Update(ctx, lease.key, payload, revision)
	switch {
	case err == nil:
		lease.mu.Lock()
		lease.revision = renewed
		lease.confirmed = time.Now()
		lease.mu.Unlock()
		return true
	case errors.Is(err, context.Canceled):
		return false
	// Update reports a lost race as ErrKeyRevisionMismatch on every replica
	// count; ErrKeyExists only ever matched it by code on a single replica.
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound):
		lease.mu.Lock()
		lease.lost = true
		lease.mu.Unlock()
		slog.Error("volume lease lost: another opener may hold this volume", "volume", lease.volume, "generation", lease.generation, "err", err)
		if onLost := lease.leases.onLost; onLost != nil {
			// WithoutCancel: release cancels this context, and the fence has
			// KV reads and a teardown to finish after that.
			go onLost(context.WithoutCancel(ctx), lease.volume, leaseLostToPeer)
		}
		return false
	default:
		// A transient JetStream error is not a lost lease. Keep renewing; the
		// TTL is several intervals wide, so there is room to recover.
		slog.Warn("volume lease: renewal failed", "volume", lease.volume, "err", err)
		return true
	}
}

// leaseOwner names this node in the lease entries it writes. A daemon with no
// NodeName is single-node by construction, but its entries still have to be
// attributable, so it says so rather than writing an empty owner.
func (cfg *Config) leaseOwner() string {
	if cfg.NodeName != "" {
		return cfg.NodeName
	}
	return "unnamed-node"
}

// acquireVolumeLease claims volumeName before an engine is opened on it. A
// daemon with no lease store cannot establish that it is the only opener, so
// it refuses rather than opening blind.
func (cfg *Config) acquireVolumeLease(ctx context.Context, volumeName string) (*volumeLease, error) {
	if cfg.leases == nil {
		return nil, fmt.Errorf("%w: cannot establish exclusive access to %s", errNoVolumeLeaseStore, volumeName)
	}
	lease, err := cfg.leases.acquire(ctx, volumeName)
	if err != nil {
		return nil, err
	}

	// Mark before a single write lands, not when a seal fails. A node killed
	// mid-write never reaches its seal, so marking there leaves no trace of the
	// case the marker exists for. A takeover writes its own richer reason.
	took, err := cfg.reportVolumeTakeover(ctx, volumeName, lease.generation)
	if err == nil && !took {
		err = cfg.markVolumeDirty(ctx, volumeName, lease.generation,
			"volume open, writes not yet confirmed to the backend")
	}
	if err != nil {
		// Opening anyway would leave writes nothing records, so a later takeover
		// could not tell this node ever held them. Give the lease back instead.
		cfg.leases.release(ctx, lease)
		return nil, fmt.Errorf("record unconfirmed writes for %s: %w", volumeName, err)
	}
	return lease, nil
}

// releaseVolumeLease gives up a lease taken by acquireVolumeLease.
func (cfg *Config) releaseVolumeLease(ctx context.Context, lease *volumeLease) {
	if cfg.leases == nil || lease == nil {
		return
	}
	cfg.leases.release(ctx, lease)
}
