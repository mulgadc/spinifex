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

	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// volumeLeaseBucket holds one entry per volume that some node has a viperblock
// engine open on. JetStream serialises the create, so the entry is the
// exclusion rather than a record of it.
const volumeLeaseBucket = "VIPERBLOCK_VOLUME_LEASES"

// ErrLeaseStoreUnavailable marks a lease claim that failed because JetStream
// could not be reached, as distinct from one that failed on its merits. The
// two are opposite conclusions: unreachable means try again shortly, whereas
// a refused claim means somebody else holds the volume.
//
// It exists because they were previously indistinguishable to the caller. A
// node restarting alongside NATS claims a lease before JetStream is serving,
// gets a deadline, and the mount is reported permanent — so a guest that only
// needed a few more seconds is failed and latched out of recovery instead.
var ErrLeaseStoreUnavailable = errors.New("volume lease store unavailable")

// leaseStoreUnavailable reports whether err means the lease store could not be
// reached at all. Deliberately a closed list of transport failures: anything
// unrecognised stays permanent, because retrying a claim that was genuinely
// refused would keep a volume wedged behind a node that cannot have it.
func leaseStoreUnavailable(err error) bool {
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, nats.ErrNoResponders),
		errors.Is(err, nats.ErrTimeout),
		errors.Is(err, nats.ErrConnectionClosed),
		errors.Is(err, nats.ErrConnectionDraining),
		errors.Is(err, nats.ErrNoServers),
		errors.Is(err, jetstream.ErrBucketNotFound):
		return true
	}
	return false
}

// volumeLeaseTTL is how long a lease outlives the node holding it. A node that
// dies mid-mount leaves its entry behind and nothing else may open the volume
// until the entry ages out.
const volumeLeaseTTL = 45 * time.Second

// volumeLeaseRenewInterval keeps a live holder's entry young. It also bounds
// how stale a confirmation can be when an outage starts, which is subtracted
// from validity to give the outage this lease survives.
const volumeLeaseRenewInterval = 10 * time.Second

// volumeLeaseRenewTimeout bounds one renewal attempt, and is set above
// deviceStallBound so a renewal that begins inside a stall can still be
// answered once the stall clears instead of being certain to fail.
const volumeLeaseRenewTimeout = 12 * time.Second

// volumeLeaseValidity is how long a holder may keep writing after its last
// *confirmed* renewal. Set volumeLeaseAcquireMargin below volumeLeaseTTL: a
// server-side TTL is not a lease unless the holder stops before the re-grant.
const volumeLeaseValidity = 40 * time.Second

// volumeLeaseAcquireMargin is the gap left between validity and the server TTL.
// It covers scheduling delay and request latency on this side, and the entry
// still has to age out before a peer can claim it.
const volumeLeaseAcquireMargin = 5 * time.Second

// deviceStallBound is the longest one stalled I/O lasts under JetStream, from
// nvme_core.io_timeout on the hosts we run. Measured as a late completion and
// never a controller reset, so a renewal that waits one out is answered.
const deviceStallBound = 10 * time.Second

// jetstreamOutageBound is the longest window of unwritable JetStream observed
// on those hosts, and the quantity the lease actually has to survive: several
// sequential writes each waiting out deviceStallBound, not one stall.
//
// Both are properties of the hosts rather than settings of ours, so neither is
// applied anywhere — they are asserted against, which is what fails the build
// when the sizing above stops covering what the storage does.
const jetstreamOutageBound = 30 * time.Second

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

	// checkEvery overrides volumeLeaseCheckInterval when set. Zero means the
	// production interval; tests shorten it to reach a deadline quickly.
	checkEvery time.Duration

	mu   sync.Mutex
	held map[string]*volumeLease
}

// checkInterval is how often a renew loop tests validity.
func (l *volumeLeases) checkInterval() time.Duration {
	if l.checkEvery > 0 {
		return l.checkEvery
	}
	return volumeLeaseCheckInterval
}

// newVolumeLeases binds the lease bucket, creating it if this is the first node
// up. owner identifies this node in the entries it writes. This bucket decides
// who may write a volume, so a single replica would put every volume in the
// cluster behind one node staying up.
func newVolumeLeases(ctx context.Context, nc *nats.Conn, owner string) (*volumeLeases, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	kv, err := kvutil.GetOrCreateBucketWithOptions(ctx, js, kvutil.BucketOptions{
		Name:        volumeLeaseBucket,
		Description: "one entry per volume with a viperblock engine open on it",
		TTL:         volumeLeaseTTL,
		History:     1,
	})
	if err != nil {
		return nil, fmt.Errorf("volume lease bucket: %w", err)
	}
	return &volumeLeases{kv: kv, owner: owner, held: make(map[string]*volumeLease)}, nil
}

// newVolumeLeasesWaiting is newVolumeLeases for a daemon that is starting. On a
// cold multi-node start every service races JetStream electing a leader, and a
// refusal that only means "not yet" must not leave this node without storage.
func newVolumeLeasesWaiting(ctx context.Context, nc *nats.Conn, owner string) (*volumeLeases, error) {
	return kvstore.OpenWithRetry(ctx, volumeLeaseBucket, kvstore.DefaultOpenWindow,
		func(ctx context.Context) (*volumeLeases, error) { return newVolumeLeases(ctx, nc, owner) })
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
	// failures counts renewals that have failed in a row, and failingSince is
	// when the run started. They exist to measure the outage: a surrender
	// always reports validity, so only a recovery can report a real length.
	failures     int
	failingSince time.Time
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
		if leaseStoreUnavailable(err) {
			return nil, fmt.Errorf("claim lease for %s: %w: %w", volumeName, err, ErrLeaseStoreUnavailable)
		}
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
	ticker := time.NewTicker(lease.leases.checkInterval())
	defer ticker.Stop()

	// The renewal runs on its own goroutine so a JetStream call that blocks for
	// its whole timeout cannot delay the validity check. Waiting for it before
	// closing done leaves release reading a settled revision.
	var renewing sync.WaitGroup
	defer func() {
		renewing.Wait()
		close(lease.done)
	}()

	// Buffered, so a renewal that finishes after the loop has gone still sends
	// its answer and exits rather than leaking on an unread channel. One slot is
	// enough because only one renewal is ever in flight.
	outcome := make(chan bool, 1)
	inFlight := false

	for {
		select {
		case <-ctx.Done():
			return
		case stillOurs := <-outcome:
			inFlight = false
			if !stillOurs {
				return
			}
		case <-ticker.C:
			if lease.expiredLocally() {
				lease.surrender(ctx)
				return
			}
			if inFlight || time.Since(lease.lastConfirmed()) < volumeLeaseRenewInterval {
				continue
			}
			inFlight = true
			renewing.Go(func() {
				outcome <- lease.renew(ctx)
			})
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
	attempts := lease.failures
	failingFor := time.Duration(0)
	if attempts > 0 {
		failingFor = time.Since(lease.failingSince)
	}
	lease.mu.Unlock()

	// unconfirmed_for_ms is pinned at validity by construction, so it cannot
	// report how long the store was unwritable. failing_for_ms is the part that
	// was observed, and it is a lower bound: the outage outlived the lease.
	slog.Error("volume lease could not be confirmed before its TTL, surrendering the volume",
		"volume", lease.volume, "generation", lease.generation,
		"unconfirmed_for_ms", otelsetup.Millis(since), "ttl_ms", otelsetup.Millis(volumeLeaseTTL),
		"attempts", attempts, "failing_for_ms", otelsetup.Millis(failingFor))

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
	updateCtx, cancel := context.WithTimeout(ctx, volumeLeaseRenewTimeout)
	defer cancel()

	renewed, err := lease.leases.kv.Update(updateCtx, lease.key, payload, revision)
	switch {
	case err == nil:
		lease.mu.Lock()
		lease.revision = renewed
		lease.confirmed = time.Now()
		attempts, outage := lease.failures, time.Since(lease.failingSince)
		lease.failures, lease.failingSince = 0, time.Time{}
		lease.mu.Unlock()
		if attempts > 0 {
			slog.Warn("volume lease: renewals recovered, so the store was unwritable for this long",
				"volume", lease.volume, "attempts", attempts, "outage_ms", otelsetup.Millis(outage))
		}
		return true
	case errors.Is(err, context.Canceled):
		return false
	// Update reports a lost race as ErrKeyRevisionMismatch on every replica
	// count; ErrKeyExists only ever matched it by code on a single replica.
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
		return lease.readopt(ctx, err)
	case errors.Is(err, jetstream.ErrKeyNotFound):
		lease.loseToPeer(ctx, err, "the entry is gone, so the volume is nobody's")
		return false
	default:
		// A transient JetStream error is not a lost lease. Keep renewing; the
		// TTL is several intervals wide, so there is room to recover.
		lease.mu.Lock()
		if lease.failures == 0 {
			lease.failingSince = time.Now()
		}
		lease.failures++
		attempts, failingFor := lease.failures, time.Since(lease.failingSince)
		lease.mu.Unlock()
		slog.Warn("volume lease: renewal failed", "volume", lease.volume,
			"consecutive", attempts, "failing_for_ms", otelsetup.Millis(failingFor), "err", err)
		return true
	}
}

// readopt decides what a rejected conditional update actually meant, and is the
// difference between fencing a volume somebody took and fencing one nobody did.
//
// A renewal that times out is treated as transient, correctly, but the server
// may have applied it anyway — leaving it one revision ahead of what this holder
// recorded, so every later attempt is refused for that reason alone. Re-reading
// separates the two: an entry still carrying this lease's owner and generation
// is this node's own write, acknowledged after the client gave up.
//
// A re-read that cannot be answered is evidence of nothing, so the lease is
// kept. Local validity is the bound that does not depend on reading anything,
// and it fences on its own schedule if the entry really has moved.
func (lease *volumeLease) readopt(ctx context.Context, cause error) bool {
	readCtx, cancel := context.WithTimeout(ctx, volumeLeaseRenewTimeout)
	defer cancel()

	entry, err := lease.leases.kv.Get(readCtx, lease.key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			lease.loseToPeer(ctx, cause, "the entry is gone, so the volume is nobody's")
			return false
		}
		slog.Warn("volume lease: a renewal was rejected and the entry could not be re-read, so the lease is kept until its validity lapses",
			"volume", lease.volume, "generation", lease.generation, "err", err, "cause", cause)
		return true
	}

	var record volumeLeaseRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		slog.Warn("volume lease: a renewal was rejected and the entry could not be parsed, so the lease is kept until its validity lapses",
			"volume", lease.volume, "generation", lease.generation, "err", err, "cause", cause)
		return true
	}

	if record.Owner != lease.leases.owner || record.Generation != lease.generation {
		lease.loseToPeer(ctx, cause, fmt.Sprintf("the entry now names owner %q generation %d", record.Owner, record.Generation))
		return false
	}

	// This lease's own entry, at a revision it never saw acknowledged. Adopt it
	// so the next renewal can succeed, but do not count it as a confirmation:
	// only a write the server acknowledged proves the entry is still ours, and
	// the validity clock is what bounds writing on a lease we cannot refresh.
	lease.mu.Lock()
	lease.revision = entry.Revision()
	lease.mu.Unlock()
	slog.Warn("volume lease: adopted a revision this node wrote but never saw acknowledged",
		"volume", lease.volume, "generation", lease.generation, "revision", entry.Revision())
	return true
}

// loseToPeer marks the lease gone and fences the export. why is for the operator: the
// two ways to lose a lease need different responses and the log line is where
// that starts.
func (lease *volumeLease) loseToPeer(ctx context.Context, cause error, why string) {
	lease.mu.Lock()
	lease.lost = true
	lease.mu.Unlock()

	slog.Error("volume lease lost: another opener may hold this volume",
		"volume", lease.volume, "generation", lease.generation, "reason", why, "err", cause)

	if onLost := lease.leases.onLost; onLost != nil {
		// WithoutCancel: release cancels this context, and the fence has KV
		// reads and a teardown to finish after that.
		go onLost(context.WithoutCancel(ctx), lease.volume, leaseLostToPeer)
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
