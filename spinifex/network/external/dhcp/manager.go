package dhcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"golang.org/x/sync/singleflight"
)

// renewJitter is the ± window applied to renewal timers to spread cluster-wide
// wake-ups and avoid piling DORA traffic on the upstream server.
const renewJitter = time.Second

// postExpiryBackoff is the cool-off applied after a re-DORA fails post
// lease expiry. Bounded — the manager keeps re-DORAing in the
// background until the server comes back (per plan §Failure modes).
const postExpiryBackoff = 30 * time.Second

// defaultAcquireSchedule is the per-attempt timeout schedule for the
// outer DORA backoff. Mirrors the deleted dhcp_manager.go retransmit
// schedule — recovers under STP convergence on a freshly-up bridge.
var defaultAcquireSchedule = []time.Duration{4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second}

// defaultAcquireBudget caps the wallclock for the full DORA loop.
const defaultAcquireBudget = 90 * time.Second

// acquireAttemptJitter is the ± window applied to each per-attempt
// timeout so concurrent acquires across vpcds don't synchronise.
const acquireAttemptJitter = time.Second

// ManagerConfig: Client + Store required. Now is for tests.
// AcquireSchedule/AcquireBudget control the outer DORA backoff;
// zero values fall back to defaults.
type ManagerConfig struct {
	Client          Client
	Store           *Store
	Now             func() time.Time
	AcquireSchedule []time.Duration
	AcquireBudget   time.Duration
}

// Manager owns active DHCP leases in vpcd: one renewal goroutine per
// BOUND lease, KV persistence, and request/reply NATS subscribers for
// daemon-side acquire/release calls.
type Manager struct {
	client          Client
	store           *Store
	now             func() time.Time
	acquireSchedule []time.Duration
	acquireBudget   time.Duration

	sf singleflight.Group

	mu           sync.Mutex
	loops        map[string]*leaseLoop
	closed       bool
	parentCtx    context.Context
	parentCancel context.CancelFunc

	wg sync.WaitGroup
}

// leaseLoop is the handle stored in Manager.loops. Pointer-identity lets
// run() distinguish its own loop from a successor (e.g. re-acquired lease
// with the same client-id).
type leaseLoop struct {
	cancel context.CancelFunc
}

// NewManager constructs a Manager. Start must be called before
// adopt-style work fires; Subscribe must be called before NATS RPCs are
// answered.
func NewManager(cfg ManagerConfig) (*Manager, error) {
	switch {
	case cfg.Client == nil:
		return nil, errors.New("dhcp manager: Client required")
	case cfg.Store == nil:
		return nil, errors.New("dhcp manager: Store required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	schedule := cfg.AcquireSchedule
	if len(schedule) == 0 {
		schedule = defaultAcquireSchedule
	}
	budget := cfg.AcquireBudget
	if budget <= 0 {
		budget = defaultAcquireBudget
	}
	return &Manager{
		client:          cfg.Client,
		store:           cfg.Store,
		now:             now,
		acquireSchedule: schedule,
		acquireBudget:   budget,
		loops:           map[string]*leaseLoop{},
	}, nil
}

// Start scans the KV bucket and spawns a renewal goroutine per live lease,
// re-issuing a RENEW to confirm the upstream binding (RFC 2131 INIT-REBOOT).
// Expired entries are deleted. Repeated calls return an error.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("dhcp manager: closed")
	}
	if m.parentCtx != nil {
		m.mu.Unlock()
		return errors.New("dhcp manager: already started")
	}
	base, cancel := context.WithCancel(ctx)
	m.parentCtx = base
	m.parentCancel = cancel
	m.mu.Unlock()

	entries, err := m.store.List()
	if err != nil {
		return fmt.Errorf("list dhcp leases: %w", err)
	}
	now := m.now()
	for _, e := range entries {
		if e.Lease == nil {
			continue
		}
		if !e.Lease.ExpiresAt().After(now) {
			if delErr := m.store.Delete(e.Lease.ClientID); delErr != nil {
				slog.Warn("dhcp manager: drop expired lease failed", "client_id", e.Lease.ClientID, "err", delErr)
			}
			continue
		}
		m.spawnLoop(e, true)
	}
	return nil
}

// Stop cancels every renewal goroutine and waits for them to exit.
// Lease state stays in KV so the next vpcd boot adopts it.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	cancel := m.parentCancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

// Subscribe registers NATS handlers under a per-AZ queue group so exactly
// one vpcd answers each request. Without a queue group all vpcds would DORA
// in parallel with the same chaddr, causing NAKs and lease leaks.
func (m *Manager) Subscribe(nc *nats.Conn) ([]*nats.Subscription, error) {
	if nc == nil {
		return nil, errors.New("dhcp manager: nats conn required")
	}
	queue := "vpcd-dhcp-workers"
	if az := m.store.AZ(); az != "" {
		queue = queue + "-" + az
	}
	type sub struct {
		topic   string
		handler nats.MsgHandler
	}
	subs := []sub{
		{TopicAcquire, m.handleAcquireMsg},
		{TopicRelease, m.handleReleaseMsg},
		{TopicDrain, m.handleDrainMsg},
	}
	var out []*nats.Subscription
	for _, s := range subs {
		ns, err := nc.QueueSubscribe(s.topic, queue, s.handler)
		if err != nil {
			for _, r := range out {
				_ = r.Unsubscribe()
			}
			return nil, fmt.Errorf("subscribe %s: %w", s.topic, err)
		}
		out = append(out, ns)
		slog.Info("dhcp manager: subscribed", "topic", s.topic, "queue", queue)
	}
	return out, nil
}

// spawnLoop registers and starts a renewal goroutine for entry.
// Replaces any pre-existing loop for the same client-id. reaffirm=true
// means do an immediate RENEW before sleeping for T1.
func (m *Manager) spawnLoop(e Entry, reaffirm bool) {
	m.mu.Lock()
	if m.closed || m.parentCtx == nil {
		m.mu.Unlock()
		return
	}
	if existing, ok := m.loops[e.Lease.ClientID]; ok {
		existing.cancel()
		delete(m.loops, e.Lease.ClientID)
	}
	loopCtx, cancel := context.WithCancel(m.parentCtx)
	loop := &leaseLoop{cancel: cancel}
	m.loops[e.Lease.ClientID] = loop
	m.wg.Add(1)
	m.mu.Unlock()

	go m.run(loopCtx, loop, e, reaffirm)
}

func (m *Manager) run(ctx context.Context, self *leaseLoop, e Entry, reaffirm bool) {
	defer m.wg.Done()
	defer func() {
		m.mu.Lock()
		if cur, ok := m.loops[e.Lease.ClientID]; ok && cur == self {
			delete(m.loops, e.Lease.ClientID)
		}
		m.mu.Unlock()
	}()

	if reaffirm {
		if err := m.doRenew(ctx, &e); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			slog.Warn("dhcp manager: startup reaffirm failed; will retry at T1", "client_id", e.Lease.ClientID, "err", err)
		}
	}

	for {
		if ctx.Err() != nil {
			return
		}
		now := m.now()
		expiry := e.Lease.ExpiresAt()
		if !expiry.After(now) {
			slog.Warn("dhcp manager: lease expired; attempting fresh DORA", "client_id", e.Lease.ClientID, "ip", e.Lease.IP)
			if err := m.doAcquire(ctx, &e); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				slog.Warn("dhcp manager: re-DORA after expiry failed; backing off", "client_id", e.Lease.ClientID, "err", err)
				if !sleepWithCtx(ctx, postExpiryBackoff) {
					return
				}
			}
			continue
		}

		renewAt := e.Lease.RenewAt()
		rebindAt := e.Lease.RebindAt()
		next := renewAt
		switch {
		case !now.Before(renewAt) && now.Before(rebindAt):
			next = rebindAt
		case !now.Before(rebindAt):
			next = expiry
		}
		if !sleepUntil(ctx, m.now, next) {
			return
		}

		now = m.now()
		switch {
		case !now.Before(expiry):
			continue
		case !now.Before(rebindAt):
			if err := m.doRenew(ctx, &e); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				slog.Warn("dhcp manager: rebind failed; waiting for expiry", "client_id", e.Lease.ClientID, "err", err)
			}
		default:
			if err := m.doRenew(ctx, &e); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				slog.Warn("dhcp manager: renew failed; will retry at T2", "client_id", e.Lease.ClientID, "err", err)
			}
		}
	}
}

func (m *Manager) doAcquire(ctx context.Context, e *Entry) error {
	lease, err := m.acquireWithBackoff(ctx, AcquireRequest{
		Bridge:      e.Lease.Bridge,
		ClientID:    e.Lease.ClientID,
		Hostname:    e.Lease.Hostname,
		VendorClass: e.Lease.VendorClass,
		HWAddr:      e.Lease.HWAddr,
	})
	if err != nil {
		return err
	}
	e.Lease = lease
	if err := m.store.Put(*e); err != nil {
		return fmt.Errorf("persist re-acquired lease: %w", err)
	}
	return nil
}

// acquireWithBackoff drives client.Acquire with per-attempt timeouts from
// the schedule, capped by AcquireBudget. Jitter prevents synchronised wakes.
// ctx.Err() short-circuits; returns last error when all attempts fail.
func (m *Manager) acquireWithBackoff(ctx context.Context, req AcquireRequest) (*Lease, error) {
	if len(m.acquireSchedule) == 0 {
		return m.client.Acquire(ctx, req)
	}
	deadline := m.now().Add(m.acquireBudget)
	var lastErr error
	for i, base := range m.acquireSchedule {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		now := m.now()
		remaining := deadline.Sub(now)
		if remaining <= 0 {
			break
		}
		attempt := base + jitter(acquireAttemptJitter)
		if attempt <= 0 {
			attempt = base
		}
		if attempt > remaining {
			attempt = remaining
		}

		attemptCtx, cancel := context.WithTimeout(ctx, attempt)
		lease, err := m.client.Acquire(attemptCtx, req)
		cancel()
		if err == nil {
			return lease, nil
		}
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		slog.Warn("dhcp manager: acquire attempt failed",
			"client_id", req.ClientID, "attempt", i+1, "of", len(m.acquireSchedule),
			"timeout", attempt, "err", err)
	}
	if lastErr == nil {
		lastErr = errors.New("acquire budget exhausted before first attempt")
	}
	return nil, fmt.Errorf("acquire after %d attempts: %w", len(m.acquireSchedule), lastErr)
}

func (m *Manager) doRenew(ctx context.Context, e *Entry) error {
	renewed, err := m.client.Renew(ctx, e.Lease)
	if err != nil {
		return err
	}
	e.Lease = renewed
	if err := m.store.Put(*e); err != nil {
		return fmt.Errorf("persist renewed lease: %w", err)
	}
	return nil
}

func (m *Manager) handleAcquireMsg(msg *nats.Msg) {
	if msg.Reply == "" {
		slog.Warn("dhcp manager: acquire request missing reply subject; dropping")
		return
	}
	var req acquireWireRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		respondAcquireErr(msg, fmt.Sprintf("decode acquire: %v", err))
		return
	}
	entry, err := m.handleAcquire(context.Background(), req)
	if err != nil {
		respondAcquireErr(msg, err.Error())
		return
	}
	body, mErr := json.Marshal(acquireWireReply{Lease: toWireLease(entry.Lease)})
	if mErr != nil {
		respondAcquireErr(msg, fmt.Sprintf("encode reply: %v", mErr))
		return
	}
	_ = msg.Respond(body)
}

func (m *Manager) handleReleaseMsg(msg *nats.Msg) {
	if msg.Reply == "" {
		slog.Warn("dhcp manager: release request missing reply subject; dropping")
		return
	}
	var req releaseWireRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		respondReleaseErr(msg, fmt.Sprintf("decode release: %v", err))
		return
	}
	clientID := req.ClientID
	if clientID == "" && req.IP != "" {
		entry, err := m.store.LookupByIP(req.PoolName, req.IP)
		switch {
		case err == nil:
			clientID = entry.Lease.ClientID
		case errors.Is(err, nats.ErrKeyNotFound):
			slog.Warn("dhcp manager: release for unknown IP", "pool", req.PoolName, "ip", req.IP)
			_ = msg.Respond(emptyReleaseReply)
			return
		default:
			respondReleaseErr(msg, fmt.Sprintf("lookup release ip: %v", err))
			return
		}
	}
	if err := m.handleRelease(context.Background(), clientID); err != nil {
		respondReleaseErr(msg, err.Error())
		return
	}
	_ = msg.Respond(emptyReleaseReply)
}

// handleAcquire is the idempotent acquire path. Concurrent requests for
// the same ClientID coalesce through singleflight so the upstream
// server sees exactly one DISCOVER even when handlers race.
func (m *Manager) handleAcquire(ctx context.Context, req acquireWireRequest) (*Entry, error) {
	if req.ClientID == "" {
		return nil, errors.New("client_id required")
	}
	result, err, _ := m.sf.Do(req.ClientID, func() (any, error) {
		return m.acquireLocked(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	entry, ok := result.(*Entry)
	if !ok {
		return nil, errors.New("acquire: unexpected singleflight result")
	}
	return entry, nil
}

func (m *Manager) acquireLocked(ctx context.Context, req acquireWireRequest) (*Entry, error) {
	existing, err := m.store.Get(req.ClientID)
	switch {
	case err == nil:
		if existing != nil && existing.Lease != nil && existing.Lease.ExpiresAt().After(m.now()) {
			return existing, nil
		}
	case errors.Is(err, nats.ErrKeyNotFound):
		// fall through to fresh acquire
	default:
		return nil, fmt.Errorf("look up lease: %w", err)
	}

	hw, err := decodeHWAddr(req.HWAddr)
	if err != nil {
		return nil, err
	}
	lease, err := m.acquireWithBackoff(ctx, AcquireRequest{
		Bridge:      req.Bridge,
		ClientID:    req.ClientID,
		Hostname:    req.Hostname,
		VendorClass: req.VendorClass,
		HWAddr:      hw,
	})
	if err != nil {
		return nil, err
	}
	entry := Entry{Purpose: req.Purpose, PoolName: req.PoolName, VPCID: req.VPCID, Lease: lease}
	if err := m.store.Put(entry); err != nil {
		return nil, fmt.Errorf("persist new lease: %w", err)
	}
	m.spawnLoop(entry, false)
	return &entry, nil
}

// DrainAll DHCPRELEASEs every lease in this vpcd's per-AZ store and deletes
// the KV records. Best-effort (per-lease failures logged, not fatal). Used on
// destructive teardown; Stop() preserves leases for adopt-on-reboot.
func (m *Manager) DrainAll(ctx context.Context) (int, error) {
	entries, err := m.store.List()
	if err != nil {
		return 0, fmt.Errorf("list leases for drain: %w", err)
	}
	released := 0
	for _, e := range entries {
		if e.Lease == nil {
			continue
		}
		if relErr := m.handleRelease(ctx, e.Lease.ClientID); relErr != nil {
			slog.Warn("dhcp manager: drain release failed", "client_id", e.Lease.ClientID, "err", relErr)
			continue
		}
		released++
	}
	slog.Info("dhcp manager: drained leases", "released", released, "total", len(entries))
	return released, nil
}

func (m *Manager) handleDrainMsg(msg *nats.Msg) {
	if msg.Reply == "" {
		slog.Warn("dhcp manager: drain request missing reply subject; dropping")
		return
	}
	n, err := m.DrainAll(context.Background())
	reply := drainWireReply{Released: n}
	if err != nil {
		reply.Error = err.Error()
	}
	body, mErr := json.Marshal(reply)
	if mErr != nil {
		slog.Warn("dhcp manager: encode drain reply failed", "err", mErr)
		return
	}
	_ = msg.Respond(body)
}

func (m *Manager) handleRelease(ctx context.Context, clientID string) error {
	if clientID == "" {
		return errors.New("client_id required")
	}
	entry, err := m.store.Get(clientID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			slog.Warn("dhcp manager: release for unknown client", "client_id", clientID)
			return nil
		}
		return fmt.Errorf("look up lease: %w", err)
	}

	m.mu.Lock()
	if loop, ok := m.loops[clientID]; ok {
		loop.cancel()
		delete(m.loops, clientID)
	}
	m.mu.Unlock()

	if err := m.client.Release(ctx, entry.Lease); err != nil {
		slog.Warn("dhcp manager: client release failed; deleting KV entry anyway", "client_id", clientID, "err", err)
	}
	return m.store.Delete(clientID)
}

// LoopCount returns the number of active renewal goroutines. Test
// hook; not part of the production surface.
func (m *Manager) LoopCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.loops)
}

// emptyReleaseReply is the pre-encoded successful release response;
// avoids repeatedly marshalling the same {} payload on the hot path.
var emptyReleaseReply = []byte(`{}`)

func respondAcquireErr(msg *nats.Msg, errMsg string) {
	body, err := json.Marshal(acquireWireReply{Error: errMsg})
	if err != nil {
		slog.Warn("dhcp manager: encode acquire error reply failed", "err", err)
		return
	}
	_ = msg.Respond(body)
}

func respondReleaseErr(msg *nats.Msg, errMsg string) {
	body, err := json.Marshal(releaseWireReply{Error: errMsg})
	if err != nil {
		slog.Warn("dhcp manager: encode release error reply failed", "err", err)
		return
	}
	_ = msg.Respond(body)
}

func decodeHWAddr(s string) (net.HardwareAddr, error) {
	if s == "" {
		return nil, nil
	}
	hw, err := net.ParseMAC(s)
	if err != nil {
		return nil, fmt.Errorf("parse hwaddr %q: %w", s, err)
	}
	return hw, nil
}

func sleepUntil(ctx context.Context, now func() time.Time, deadline time.Time) bool {
	d := deadline.Sub(now())
	if d > 0 {
		d += jitter(renewJitter)
		if d < 0 {
			d = 0
		}
	} else {
		d = 0
	}
	return sleepWithCtx(ctx, d)
}

func sleepWithCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func jitter(span time.Duration) time.Duration {
	if span <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(span*2))) - span //nolint:gosec // jitter, not cryptographic
}
