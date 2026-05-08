package vpcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/dhcp"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// DHCPManager owns the active DHCP leases held by spinifex-vpcd. It
// services vpc.dhcp.acquire / vpc.dhcp.release NATS requests from the
// daemon-side ExternalIPAM handlers, runs a per-lease renewal goroutine,
// and persists each lease in the spinifex-dhcp-leases KV bucket so that
// a vpcd restart does not forget what has already been allocated upstream.
type DHCPManager struct {
	client dhcp.Client
	kv     nats.KeyValue
	nc     *nats.Conn

	// macForClientID produces the chaddr for a request that came in with
	// HWAddr unset. Defaults to generateMAC; tests override.
	macForClientID func(clientID string) net.HardwareAddr

	// acquireTimeout is the wallclock budget for a full Acquire — sum of
	// every retransmit attempt in acquireBackoff plus slack. Renew/Release
	// reuse this as their single-attempt deadline.
	acquireTimeout time.Duration

	// acquireBackoff drives the per-DISCOVER timeouts used by
	// acquireWithBackoff (RFC 2131 §4.1 retransmission). Each entry is
	// the wait-for-OFFER window for one attempt; the next entry doubles.
	// Default: 4s, 8s, 16s, 32s. ±1s jitter applied per attempt.
	acquireBackoff []time.Duration

	// jitterFraction is the ± range applied to renewal sleeps, expressed
	// as a fraction of T1 (0.1 = ±10%).
	jitterFraction float64
	rng            *rand.Rand

	// promiscOn/Off are called to toggle IFF_PROMISC on the DHCP bridge
	// around each Acquire. nil = no-op (non-veth modes, or tests).
	// promiscCount tracks in-flight acquires so the bridge is not cleared
	// while a concurrent DORA is still waiting for a reply.
	promiscOn    func()
	promiscOff   func()
	promiscMu    sync.Mutex
	promiscCount int

	mu     sync.Mutex
	leases map[string]*managedLease // keyed by ClientID

	// inFlightMu guards inFlight. Tracks concurrent Acquire calls per
	// bridge so handleAcquire can surface contention as a diagnostic
	// signal (mulga-siv-32).
	inFlightMu sync.Mutex
	inFlight   map[string]int

	stopCtx    context.Context
	stopCancel context.CancelFunc
	wg         sync.WaitGroup
}

type managedLease struct {
	lease     *dhcp.Lease
	renewStop context.CancelFunc
	renewDone chan struct{}
}

// DHCPManagerOption configures DHCPManager at construction.
type DHCPManagerOption func(*DHCPManager)

// WithDHCPMACFunc overrides the default MAC derivation (generateMAC).
func WithDHCPMACFunc(fn func(clientID string) net.HardwareAddr) DHCPManagerOption {
	return func(m *DHCPManager) { m.macForClientID = fn }
}

// WithDHCPAcquireTimeout overrides the wallclock budget for a full Acquire
// (sum of every retransmit attempt + slack). Renew/Release reuse it.
func WithDHCPAcquireTimeout(d time.Duration) DHCPManagerOption {
	return func(m *DHCPManager) { m.acquireTimeout = d }
}

// WithDHCPAcquireBackoff overrides the per-DISCOVER timeout schedule used
// by acquireWithBackoff. Tests use a tight schedule; production keeps the
// RFC 2131 §4.1 default (4s, 8s, 16s, 32s with ±1s jitter).
func WithDHCPAcquireBackoff(b []time.Duration) DHCPManagerOption {
	return func(m *DHCPManager) { m.acquireBackoff = b }
}

// WithDHCPJitterFraction overrides the ±fraction applied to renewal sleeps.
func WithDHCPJitterFraction(f float64) DHCPManagerOption {
	return func(m *DHCPManager) { m.jitterFraction = f }
}

// WithDHCPRand overrides the random source used for jitter (tests only).
func WithDHCPRand(r *rand.Rand) DHCPManagerOption {
	return func(m *DHCPManager) { m.rng = r }
}

// WithDHCPPromiscFuncs wires IFF_PROMISC toggling around each Acquire. on is
// called before the DORA and off is called after (even on error). Both are
// reference-counted so concurrent acquires keep the bridge promisc until the
// last one completes. Pass nil to disable (default).
func WithDHCPPromiscFuncs(on, off func()) DHCPManagerOption {
	return func(m *DHCPManager) {
		m.promiscOn = on
		m.promiscOff = off
	}
}

// NewDHCPManager creates a Manager wired against js for KV persistence and
// nc for NATS request/reply. The caller still needs to call Bootstrap (to
// rehydrate persisted leases) and Subscribe (to start servicing requests).
func NewDHCPManager(nc *nats.Conn, js nats.JetStreamContext, client dhcp.Client, opts ...DHCPManagerOption) (*DHCPManager, error) {
	if nc == nil {
		return nil, fmt.Errorf("DHCPManager: nats.Conn is required")
	}
	if js == nil {
		return nil, fmt.Errorf("DHCPManager: JetStreamContext is required")
	}
	if client == nil {
		return nil, fmt.Errorf("DHCPManager: dhcp.Client is required")
	}

	kv, err := utils.GetOrCreateKVBucket(js, dhcp.KVBucketDHCPLeases, 3)
	if err != nil {
		return nil, fmt.Errorf("create DHCP lease KV bucket: %w", err)
	}
	if err := migrate.DefaultRegistry.RunKV(dhcp.KVBucketDHCPLeases, kv, dhcp.KVBucketDHCPLeasesVersion); err != nil {
		return nil, fmt.Errorf("migrate %s: %w", dhcp.KVBucketDHCPLeases, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &DHCPManager{
		client: client,
		kv:     kv,
		nc:     nc,
		// 90s = sum of default acquireBackoff (4+8+16+32 = 60s) + slack for
		// socket open/close on each attempt and the final ACK round-trip.
		acquireTimeout: 90 * time.Second,
		acquireBackoff: []time.Duration{
			4 * time.Second,
			8 * time.Second,
			16 * time.Second,
			32 * time.Second,
		},
		jitterFraction: 0.1,
		rng:            rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0xdeadbeef)), //nolint:gosec // non-cryptographic jitter
		leases:         map[string]*managedLease{},
		inFlight:       map[string]int{},
		stopCtx:        ctx,
		stopCancel:     cancel,
		macForClientID: func(id string) net.HardwareAddr {
			mac, err := net.ParseMAC(generateMAC(id))
			if err != nil {
				return nil
			}
			return mac
		},
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Bootstrap reads every persisted lease from KV and schedules the
// renewal goroutine. Leases already past their expiry are removed and
// a TopicLeaseExpired event is published so the reconcilers upstream
// know they need to re-request.
func (m *DHCPManager) Bootstrap(ctx context.Context) error {
	keys, err := m.kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			slog.Info("DHCP Manager bootstrapped", "leases", 0)
			return nil
		}
		return fmt.Errorf("list DHCP lease keys: %w", err)
	}

	now := time.Now()
	resumed, expired := 0, 0
	for _, key := range keys {
		if key == "_version" {
			continue
		}
		entry, err := m.kv.Get(key)
		if err != nil {
			slog.Warn("DHCP Manager: failed to load lease", "client_id", key, "err", err)
			continue
		}
		lease, err := decodeLease(entry.Value())
		if err != nil {
			slog.Warn("DHCP Manager: failed to decode lease", "client_id", key, "err", err)
			continue
		}

		if now.After(lease.ExpiresAt()) {
			_ = m.kv.Delete(key)
			m.publishExpired(lease, "", "bootstrap_past_expiry")
			expired++
			continue
		}

		m.trackAndRenew(lease)
		resumed++
	}
	slog.Info("DHCP Manager bootstrapped", "leases", resumed, "expired", expired)
	return nil
}

// Subscribe registers queue-subscribed handlers on vpc.dhcp.acquire and
// vpc.dhcp.release. Queue group "vpcd-dhcp-workers" matches the existing
// vpcd-workers convention.
func (m *DHCPManager) Subscribe(nc *nats.Conn) ([]*nats.Subscription, error) {
	var subs []*nats.Subscription

	acquire, err := nc.QueueSubscribe(dhcp.TopicAcquire, "vpcd-dhcp-workers", m.handleAcquire)
	if err != nil {
		return nil, fmt.Errorf("subscribe %s: %w", dhcp.TopicAcquire, err)
	}
	subs = append(subs, acquire)
	slog.Info("DHCP Manager subscribed", "topic", dhcp.TopicAcquire)

	release, err := nc.QueueSubscribe(dhcp.TopicRelease, "vpcd-dhcp-workers", m.handleRelease)
	if err != nil {
		_ = acquire.Unsubscribe()
		return nil, fmt.Errorf("subscribe %s: %w", dhcp.TopicRelease, err)
	}
	subs = append(subs, release)
	slog.Info("DHCP Manager subscribed", "topic", dhcp.TopicRelease)

	return subs, nil
}

// Close stops all renewal goroutines and waits for them to exit.
func (m *DHCPManager) Close() {
	m.stopCancel()
	m.wg.Wait()
}

// handleAcquire processes a vpc.dhcp.acquire request. Idempotent: if we
// already hold a valid lease for the requested ClientID we return it
// without a fresh DORA. Lets the daemon-side CAS retry loop be safe.
func (m *DHCPManager) handleAcquire(msg *nats.Msg) {
	var req dhcp.AcquireRequestMsg
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		m.replyAcquireError(msg, fmt.Sprintf("invalid acquire request: %v", err))
		return
	}
	if req.Bridge == "" || req.ClientID == "" {
		m.replyAcquireError(msg, "acquire: bridge and client_id are required")
		return
	}

	if existing, ok := m.get(req.ClientID); ok {
		if time.Now().Before(existing.ExpiresAt()) {
			m.replyAcquire(msg, existing)
			return
		}
		m.forget(req.ClientID, "stale_existing")
	}

	hwAddr, err := resolveHWAddr(req.HWAddr, req.ClientID, m.macForClientID)
	if err != nil {
		m.replyAcquireError(msg, err.Error())
		return
	}

	m.enablePromisc()
	defer m.disablePromisc()

	ctx, cancel := context.WithTimeout(m.stopCtx, m.acquireTimeout)
	defer cancel()

	// Diagnostic for mulga-siv-32: track concurrent Acquire calls per
	// bridge and the wallclock cost of each DORA. Concurrent DISCOVERs on
	// the same bridge are a suspected contributor to nightly DORA timeouts.
	inFlight := m.beginAcquire(req.Bridge)
	defer m.endAcquire(req.Bridge)
	start := time.Now()

	lease, err := m.acquireWithBackoff(ctx, dhcp.AcquireRequest{
		Bridge:      req.Bridge,
		ClientID:    req.ClientID,
		Hostname:    req.Hostname,
		VendorClass: req.VendorClass,
		HWAddr:      hwAddr,
	})
	if err != nil {
		slog.Warn("DHCP Manager: acquire failed",
			"client_id", req.ClientID, "bridge", req.Bridge,
			"in_flight_at_start", inFlight, "duration", time.Since(start),
			"err", err)
		m.replyAcquireError(msg, err.Error())
		return
	}
	slog.Debug("DHCP Manager: acquire ok",
		"client_id", req.ClientID, "bridge", req.Bridge,
		"in_flight_at_start", inFlight, "duration", time.Since(start))

	if err := m.persistLease(lease); err != nil {
		slog.Warn("DHCP Manager: persist lease failed; lease will not survive restart", "client_id", req.ClientID, "err", err)
	}
	m.trackAndRenew(lease)

	slog.Info("DHCP Manager acquired lease",
		"client_id", lease.ClientID, "ip", lease.IP.String(),
		"bridge", lease.Bridge, "server_id", lease.ServerID.String(),
		"expires_in", time.Until(lease.ExpiresAt()))

	m.replyAcquire(msg, lease)
}

// handleRelease processes a vpc.dhcp.release request.
func (m *DHCPManager) handleRelease(msg *nats.Msg) {
	var req dhcp.ReleaseRequestMsg
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		m.replyRelease(msg, fmt.Sprintf("invalid release request: %v", err))
		return
	}
	if req.ClientID == "" {
		m.replyRelease(msg, "release: client_id is required")
		return
	}

	lease, ok := m.get(req.ClientID)
	if !ok {
		// No active lease; treat as a no-op so the daemon can call release
		// in clean-up paths without worrying about order.
		m.replyRelease(msg, "")
		return
	}

	ctx, cancel := context.WithTimeout(m.stopCtx, m.acquireTimeout)
	defer cancel()
	if err := m.client.Release(ctx, lease); err != nil {
		slog.Warn("DHCP Manager: release failed", "client_id", req.ClientID, "err", err)
		// Still forget locally — upstream server may or may not have seen
		// the RELEASE, but holding on to a lease we've been told to drop
		// serves no purpose.
	}
	m.forget(req.ClientID, "released")
	slog.Info("DHCP Manager released lease", "client_id", req.ClientID)
	m.replyRelease(msg, "")
}

// enablePromisc increments the in-flight acquire count and enables IFF_PROMISC
// on the first call.
func (m *DHCPManager) enablePromisc() {
	if m.promiscOn == nil {
		return
	}
	m.promiscMu.Lock()
	defer m.promiscMu.Unlock()
	if m.promiscCount == 0 {
		m.promiscOn()
	}
	m.promiscCount++
}

// disablePromisc decrements the count and disables IFF_PROMISC when it reaches
// zero (i.e. no more in-flight acquires need it).
func (m *DHCPManager) disablePromisc() {
	if m.promiscOff == nil {
		return
	}
	m.promiscMu.Lock()
	defer m.promiscMu.Unlock()
	m.promiscCount--
	if m.promiscCount == 0 {
		m.promiscOff()
	}
}

// trackAndRenew registers the lease in the in-memory map and starts the
// per-lease renewal goroutine. Replaces any prior tracker for the same
// ClientID.
func (m *DHCPManager) trackAndRenew(lease *dhcp.Lease) {
	m.mu.Lock()
	if prev, ok := m.leases[lease.ClientID]; ok && prev.renewStop != nil {
		prev.renewStop()
		m.mu.Unlock()
		<-prev.renewDone
		m.mu.Lock()
	}

	// cancel is stored in tracker.renewStop and fired from forget() or Close().
	ctx, cancel := context.WithCancel(m.stopCtx) //nolint:gosec // G118: cancel stored in tracker.renewStop, fired via forget()/Close()
	tracker := &managedLease{
		lease:     lease,
		renewStop: cancel,
		renewDone: make(chan struct{}),
	}
	m.leases[lease.ClientID] = tracker
	m.mu.Unlock()

	m.wg.Add(1)
	go m.renewLoop(ctx, tracker)
}

// renewLoop drives one lease's renewal lifecycle. Sleeps until T1 (with
// ±jitter), attempts Renew; on failure retries at T2; on terminal failure
// past the expiry, publishes TopicLeaseExpired and forgets the lease.
func (m *DHCPManager) renewLoop(ctx context.Context, tracker *managedLease) {
	defer m.wg.Done()
	defer close(tracker.renewDone)

	for {
		lease := m.snapshotLease(tracker)
		if lease == nil {
			return
		}

		// Phase 1 — wait for T1 (± jitter) and try RENEW.
		wait := m.jitter(time.Until(lease.RenewAt()))
		if !sleep(ctx, wait) {
			return
		}

		renewed, err := m.tryRenew(ctx, lease)
		if err == nil {
			tracker.lease = renewed
			if perr := m.persistLease(renewed); perr != nil {
				slog.Warn("DHCP Manager: persist renewed lease failed", "client_id", renewed.ClientID, "err", perr)
			}
			continue
		}
		slog.Warn("DHCP Manager: renew failed, will retry at T2", "client_id", lease.ClientID, "err", err)

		// Phase 2 — retry until T2.
		wait = m.jitter(time.Until(lease.RebindAt()))
		if !sleep(ctx, wait) {
			return
		}
		renewed, err = m.tryRenew(ctx, lease)
		if err == nil {
			tracker.lease = renewed
			if perr := m.persistLease(renewed); perr != nil {
				slog.Warn("DHCP Manager: persist rebound lease failed", "client_id", renewed.ClientID, "err", perr)
			}
			continue
		}

		// Phase 3 — wait out the rest of the lease; on expiry, emit
		// lease-expired and drop.
		wait = time.Until(lease.ExpiresAt())
		if !sleep(ctx, wait) {
			return
		}
		slog.Warn("DHCP Manager: lease expired without renewal", "client_id", lease.ClientID, "ip", lease.IP.String(), "err", err)
		m.publishExpired(lease, "", classifyRenewErr(err))
		m.forget(lease.ClientID, "expired")
		return
	}
}

// acquireWithBackoff drives RFC 2131 §4.1 DISCOVER retransmission. Each
// entry in acquireBackoff sets one attempt's per-OFFER wait window with
// ±1s jitter; on timeout the loop tries the next entry. Returns on first
// success, on parent ctx cancellation, or after the schedule is exhausted.
//
// Real DHCP servers (consumer routers, ISC dhcpd, dnsmasq) silently drop
// OFFERs under load — without retransmission a single lost packet kills
// the whole DORA. mulga-siv-39.
func (m *DHCPManager) acquireWithBackoff(parent context.Context, req dhcp.AcquireRequest) (*dhcp.Lease, error) {
	schedule := m.acquireBackoff
	if len(schedule) == 0 {
		// Defensive: caller passed an empty schedule via WithDHCPAcquireBackoff.
		// Fall through to a single attempt bounded by parent ctx.
		schedule = []time.Duration{0}
	}

	var lastErr error
	for i, base := range schedule {
		if err := parent.Err(); err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("dhcp acquire on %s (client=%s): %w (last attempt err: %v)", req.Bridge, req.ClientID, err, lastErr)
			}
			return nil, fmt.Errorf("dhcp acquire on %s (client=%s): %w", req.Bridge, req.ClientID, err)
		}

		attemptTimeout := m.dhcpAttemptTimeout(base)
		var (
			ctx    context.Context
			cancel context.CancelFunc
		)
		if attemptTimeout > 0 {
			ctx, cancel = context.WithTimeout(parent, attemptTimeout)
		} else {
			ctx, cancel = context.WithCancel(parent)
		}

		lease, err := m.client.Acquire(ctx, req)
		cancel()
		if err == nil {
			return lease, nil
		}
		lastErr = err

		if i < len(schedule)-1 {
			slog.Debug("DHCP Manager: DORA retransmit",
				"client_id", req.ClientID, "bridge", req.Bridge,
				"attempt", i+1, "next_timeout", m.dhcpAttemptTimeout(schedule[i+1]),
				"prev_err", err)
		}
	}
	return nil, lastErr
}

// dhcpAttemptTimeout applies ±1s jitter to a base DISCOVER timeout.
// Pass 0 to disable timeout (used when schedule is empty fallback).
func (m *DHCPManager) dhcpAttemptTimeout(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	jitter := time.Duration((m.rng.Float64()*2 - 1) * float64(time.Second))
	out := base + jitter
	if out <= 0 {
		return base
	}
	return out
}

// tryRenew calls the backend Renew under a bounded context.
func (m *DHCPManager) tryRenew(parent context.Context, lease *dhcp.Lease) (*dhcp.Lease, error) {
	ctx, cancel := context.WithTimeout(parent, m.acquireTimeout)
	defer cancel()
	return m.client.Renew(ctx, lease)
}

// beginAcquire increments the in-flight Acquire counter for bridge and
// returns the count after increment (>= 1). Diagnostic for mulga-siv-32.
func (m *DHCPManager) beginAcquire(bridge string) int {
	m.inFlightMu.Lock()
	defer m.inFlightMu.Unlock()
	m.inFlight[bridge]++
	return m.inFlight[bridge]
}

// endAcquire decrements the in-flight Acquire counter for bridge.
func (m *DHCPManager) endAcquire(bridge string) {
	m.inFlightMu.Lock()
	defer m.inFlightMu.Unlock()
	if m.inFlight[bridge] > 0 {
		m.inFlight[bridge]--
	}
}

// snapshotLease returns the tracker's current lease under the lock.
func (m *DHCPManager) snapshotLease(tracker *managedLease) *dhcp.Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	return tracker.lease
}

// get returns the current lease for a ClientID, if tracked.
func (m *DHCPManager) get(clientID string) (*dhcp.Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.leases[clientID]
	if !ok {
		return nil, false
	}
	return t.lease, true
}

// forget removes a lease from the tracking map, KV and stops its renewal
// goroutine.
func (m *DHCPManager) forget(clientID, reason string) {
	m.mu.Lock()
	t, ok := m.leases[clientID]
	if ok {
		delete(m.leases, clientID)
	}
	m.mu.Unlock()

	if ok && t.renewStop != nil {
		t.renewStop()
	}
	if err := m.kv.Delete(clientID); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		slog.Debug("DHCP Manager: delete KV key failed", "client_id", clientID, "reason", reason, "err", err)
	}
}

// persistLease writes the lease state to the KV bucket under ClientID.
func (m *DHCPManager) persistLease(lease *dhcp.Lease) error {
	data, err := encodeLease(lease)
	if err != nil {
		return err
	}
	if _, err := m.kv.Put(lease.ClientID, data); err != nil {
		return fmt.Errorf("put lease %s: %w", lease.ClientID, err)
	}
	return nil
}

func (m *DHCPManager) publishExpired(lease *dhcp.Lease, poolName, reason string) {
	evt := dhcp.LeaseExpiredEvent{
		ClientID: lease.ClientID,
		PoolName: poolName,
		IP:       lease.IP.String(),
		Reason:   reason,
	}
	data, err := json.Marshal(evt)
	if err != nil {
		slog.Warn("DHCP Manager: marshal lease-expired event failed", "client_id", lease.ClientID, "err", err)
		return
	}
	if err := m.nc.Publish(dhcp.TopicLeaseExpired, data); err != nil {
		slog.Warn("DHCP Manager: publish lease-expired failed", "client_id", lease.ClientID, "err", err)
	}
}

func (m *DHCPManager) replyAcquire(msg *nats.Msg, lease *dhcp.Lease) {
	reply := dhcp.AcquireReplyMsg{
		IP:          lease.IP.String(),
		SubnetMask:  net.IP(lease.SubnetMask).String(),
		Routers:     ipsToStrings(lease.Routers),
		DNS:         ipsToStrings(lease.DNS),
		ServerID:    lease.ServerID.String(),
		HWAddr:      lease.HWAddr.String(),
		ExpiresUnix: lease.ExpiresAt().Unix(),
	}
	respondJSON(msg, reply)
}

func (m *DHCPManager) replyAcquireError(msg *nats.Msg, errMsg string) {
	respondJSON(msg, dhcp.AcquireReplyMsg{Error: errMsg})
}

func (m *DHCPManager) replyRelease(msg *nats.Msg, errMsg string) {
	respondJSON(msg, dhcp.ReleaseReplyMsg{Error: errMsg})
}

// jitter applies ±jitterFraction of d to produce an actual sleep duration.
// Negative d is clamped to 0.
func (m *DHCPManager) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if m.jitterFraction <= 0 {
		return d
	}
	spread := float64(d) * m.jitterFraction
	offset := (m.rng.Float64()*2 - 1) * spread
	out := time.Duration(float64(d) + offset)
	if out < 0 {
		return 0
	}
	return out
}

// resolveHWAddr parses the hw_addr string supplied in the NATS request,
// falling back to a derived MAC when the request carries no value.
func resolveHWAddr(raw, clientID string, derive func(string) net.HardwareAddr) (net.HardwareAddr, error) {
	if raw != "" {
		mac, err := net.ParseMAC(raw)
		if err != nil {
			return nil, fmt.Errorf("parse hw_addr %q: %w", raw, err)
		}
		return mac, nil
	}
	if derive == nil {
		return nil, fmt.Errorf("hw_addr is empty and no derivation function configured")
	}
	mac := derive(clientID)
	if len(mac) == 0 {
		return nil, fmt.Errorf("derive hw_addr for client %q: empty result", clientID)
	}
	return mac, nil
}

// classifyRenewErr maps a renew error to a short reason code on the
// lease-expired event. Kept coarse; callers that care about specifics
// can re-log from their side.
func classifyRenewErr(err error) string {
	if err == nil {
		return "unknown"
	}
	// nclient4 returns *nclient4.ErrNak on NAK. Rather than taking a
	// dependency on its private type here, match the known message.
	msg := err.Error()
	if strings.Contains(msg, "NAK") || strings.Contains(msg, "Nak") {
		return "nak"
	}
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "i/o timeout") {
		return "server_unreachable"
	}
	return "renew_failed"
}

func ipsToStrings(ips []net.IP) []string {
	if len(ips) == 0 {
		return nil
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

// respondJSON marshals v and sends it as the reply. If the message has
// no Reply subject (fire-and-forget), drops the reply silently.
func respondJSON(msg *nats.Msg, v any) {
	if msg.Reply == "" {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		slog.Warn("DHCP Manager: marshal reply failed", "err", err)
		return
	}
	if err := msg.Respond(data); err != nil {
		slog.Warn("DHCP Manager: respond failed", "err", err)
	}
}

// sleep blocks until d elapses or ctx is done. Returns false if ctx was
// cancelled (caller should exit).
func sleep(ctx context.Context, d time.Duration) bool {
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

// --- KV encoding ---

// leaseRecord is the JSON representation persisted in KV and used to
// rehydrate on vpcd startup. Explicit format rather than gob-encoding
// *dhcp.Lease so a future Go type change doesn't invalidate every
// in-flight lease.
type leaseRecord struct {
	Version      int      `json:"version"`
	Bridge       string   `json:"bridge"`
	ClientID     string   `json:"client_id"`
	Hostname     string   `json:"hostname,omitempty"`
	VendorClass  string   `json:"vendor_class,omitempty"`
	HWAddr       string   `json:"hw_addr"`
	IP           string   `json:"ip"`
	SubnetMask   string   `json:"subnet_mask,omitempty"`
	Routers      []string `json:"routers,omitempty"`
	DNS          []string `json:"dns,omitempty"`
	ServerID     string   `json:"server_id,omitempty"`
	AcquiredUnix int64    `json:"acquired_unix"`
	LeaseSeconds int64    `json:"lease_seconds"`
	T1Seconds    int64    `json:"t1_seconds"`
	T2Seconds    int64    `json:"t2_seconds"`
	RawOfferB64  []byte   `json:"raw_offer,omitempty"`
	RawACKB64    []byte   `json:"raw_ack,omitempty"`
}

const leaseRecordVersion = 1

func encodeLease(l *dhcp.Lease) ([]byte, error) {
	rec := leaseRecord{
		Version:      leaseRecordVersion,
		Bridge:       l.Bridge,
		ClientID:     l.ClientID,
		Hostname:     l.Hostname,
		VendorClass:  l.VendorClass,
		HWAddr:       l.HWAddr.String(),
		IP:           l.IP.String(),
		SubnetMask:   maskString(l.SubnetMask),
		Routers:      ipsToStrings(l.Routers),
		DNS:          ipsToStrings(l.DNS),
		ServerID:     l.ServerID.String(),
		AcquiredUnix: l.AcquiredAt.Unix(),
		LeaseSeconds: int64(l.LeaseDuration / time.Second),
		T1Seconds:    int64(l.T1 / time.Second),
		T2Seconds:    int64(l.T2 / time.Second),
		RawOfferB64:  l.RawOffer,
		RawACKB64:    l.RawACK,
	}
	return json.Marshal(rec)
}

func decodeLease(data []byte) (*dhcp.Lease, error) {
	var rec leaseRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal lease: %w", err)
	}
	if rec.Version != leaseRecordVersion {
		return nil, fmt.Errorf("unsupported lease record version %d", rec.Version)
	}
	mac, err := net.ParseMAC(rec.HWAddr)
	if err != nil {
		return nil, fmt.Errorf("parse hw_addr %q: %w", rec.HWAddr, err)
	}
	return &dhcp.Lease{
		Bridge:        rec.Bridge,
		ClientID:      rec.ClientID,
		Hostname:      rec.Hostname,
		VendorClass:   rec.VendorClass,
		HWAddr:        mac,
		IP:            net.ParseIP(rec.IP),
		SubnetMask:    parseMask(rec.SubnetMask),
		Routers:       parseIPs(rec.Routers),
		DNS:           parseIPs(rec.DNS),
		ServerID:      net.ParseIP(rec.ServerID),
		AcquiredAt:    time.Unix(rec.AcquiredUnix, 0),
		LeaseDuration: time.Duration(rec.LeaseSeconds) * time.Second,
		T1:            time.Duration(rec.T1Seconds) * time.Second,
		T2:            time.Duration(rec.T2Seconds) * time.Second,
		RawOffer:      rec.RawOfferB64,
		RawACK:        rec.RawACKB64,
	}, nil
}

func maskString(m net.IPMask) string {
	if len(m) == 0 {
		return ""
	}
	return net.IP(m).String()
}

func parseMask(s string) net.IPMask {
	ip := net.ParseIP(s)
	if ip == nil {
		return nil
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}
	return net.IPv4Mask(ip4[0], ip4[1], ip4[2], ip4[3])
}

func parseIPs(ss []string) []net.IP {
	if len(ss) == 0 {
		return nil
	}
	out := make([]net.IP, 0, len(ss))
	for _, s := range ss {
		if ip := net.ParseIP(s); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}
