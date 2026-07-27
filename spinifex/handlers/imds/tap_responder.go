package handlers_imds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// tapListenFunc binds a TCP listener on 169.254.169.254:80 to the per-tap
// endpoint device via SO_BINDTODEVICE. Swapped in tests to avoid the privileges
// and link-local address a real endpoint needs.
type tapListenFunc func(ctx context.Context, endpoint string) (net.Listener, error)

// resolveENIFunc resolves a tap's ENI identity from its ENI ID. Injected so the
// responder manager is unit-testable without a live ENI bucket.
type resolveENIFunc func(ctx context.Context, eniID string) (*eniFacts, error)

// errENIRecordGone marks a start failure caused by a definitively absent ENI
// record (as opposed to a transient resolve error). reconcile suspends retries
// for such ENIs so a torn-down instance does not churn the log every pass.
var errENIRecordGone = errors.New("ENI record gone")

// ifindexFunc resolves an endpoint device's current kernel ifindex. Injected so
// the recreated-endpoint check is unit-testable without real devices.
type ifindexFunc func(dev string) (int, error)

// activeTapResponder is one tap's realised serving state: a listener bound to the
// tap's endpoint on br-imds and the http.Server serving it with the tap's ENI.
// endpoint + ifindex pin which device the listener is bound to (via
// SO_BINDTODEVICE), so reconcile can spot a torn-down/recreated endpoint.
type activeTapResponder struct {
	listener net.Listener
	server   *http.Server
	endpoint string
	ifindex  int
	// DNS shim sockets on 169.254.169.253:53, nil until bound (the shim is
	// optional and its bind failures retry without affecting IMDS serving).
	dnsUDP net.PacketConn
	dnsTCP net.Listener
}

// closeDNS closes the responder's DNS shim sockets, if bound.
func (r *activeTapResponder) closeDNS() {
	if r.dnsUDP != nil {
		_ = r.dnsUDP.Close()
	}
	if r.dnsTCP != nil {
		_ = r.dnsTCP.Close()
	}
}

// tapResponderManager runs one IMDS responder per local primary-ENI tap. Each
// binds the shared handler to the tap's endpoint via SO_BINDTODEVICE and serves
// it with the tap's ENI (resolved once), so overlapping guest CIDRs never collide.
type tapResponderManager struct {
	handler http.Handler
	resolve resolveENIFunc
	listen  tapListenFunc
	ifindex ifindexFunc

	// DNS shim (VPC DNS co-tenant). Both nil unless enableDNS was called.
	dnsListen dnsListenFunc
	dnsFwd    *dnsForwarder

	mu      sync.Mutex
	active  map[string]*activeTapResponder // eniID → responder
	missing map[string]struct{}            // eniID → record gone, retries suspended
}

func newTapResponderManager(handler http.Handler, resolve resolveENIFunc, listen tapListenFunc) *tapResponderManager {
	return &tapResponderManager{
		handler: handler,
		resolve: resolve,
		listen:  listen,
		ifindex: deviceIfindex,
		active:  make(map[string]*activeTapResponder),
		missing: make(map[string]struct{}),
	}
}

// start resolves the tap's ENI once, binds a listener to its endpoint, and serves
// the shared handler with that identity. Idempotent per ENI. A missing ENI record
// is a retryable error — it is written before the tap exists, so a miss is transient.
func (m *tapResponderManager) start(ctx context.Context, eniID, endpoint string) error {
	if eniID == "" || endpoint == "" {
		return errors.New("tap responder: eniID and endpoint required")
	}

	m.mu.Lock()
	cur, ok := m.active[eniID]
	m.mu.Unlock()
	if ok {
		if !m.endpointRecreated(cur, endpoint) {
			m.ensureDNS(ctx, eniID, endpoint)
			return nil
		}
		// A stop/start faster than the reconcile interval recreates the endpoint
		// device (same ENI-derived name, fresh ifindex) without reconcile ever
		// seeing the gap, so this stale listener stays bound to the deleted device
		// via SO_BINDTODEVICE and serves nothing. Drop it and rebind to the live one.
		slog.InfoContext(ctx, "IMDS: tap endpoint recreated, rebinding responder", "eni_id", eniID, "endpoint", endpoint)
		m.stop(eniID)
	}

	eni, err := m.resolve(ctx, eniID)
	if err != nil {
		return fmt.Errorf("resolve eni %s: %w", eniID, err)
	}
	if eni == nil {
		return fmt.Errorf("no ENI record for %s: %w", eniID, errENIRecordGone)
	}

	listener, err := m.listen(ctx, endpoint)
	if err != nil {
		return fmt.Errorf("listen on endpoint %s: %w", endpoint, err)
	}
	ifindex, err := m.ifindex(endpoint)
	if err != nil {
		slog.DebugContext(ctx, "IMDS: endpoint ifindex unavailable; recreated-endpoint detection degraded", "endpoint", endpoint, "err", err)
	}

	server := &http.Server{
		Handler:           m.handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return context.WithValue(ctx, ctxKeyENI, eni)
		},
	}

	m.mu.Lock()
	if _, ok := m.active[eniID]; ok { // re-check under lock
		m.mu.Unlock()
		_ = listener.Close()
		return nil
	}
	m.active[eniID] = &activeTapResponder{listener: listener, server: server, endpoint: endpoint, ifindex: ifindex}
	m.mu.Unlock()

	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			return // clean stop/shutdown already removed the entry
		}
		// Unexpected exit: drop ourselves so the next reconcile re-starts this tap;
		// otherwise the stale entry makes start a no-op and the tap never serves again.
		slog.ErrorContext(ctx, "IMDS: tap responder serve exited", "eni_id", eniID, "endpoint", endpoint, "err", err)
		m.removeIfCurrent(eniID, server)
	}()

	slog.InfoContext(ctx, "IMDS: tap responder serving", "eni_id", eniID, "endpoint", endpoint, "addr", listener.Addr().String())
	m.ensureDNS(ctx, eniID, endpoint)
	return nil
}

// enableDNS turns on the per-tap VPC DNS shim: every responder additionally
// binds 169.254.169.253:53 on its endpoint and relays queries via fwd. Must be
// called before the manager starts reconciling.
func (m *tapResponderManager) enableDNS(listen dnsListenFunc, fwd *dnsForwarder) {
	m.dnsListen = listen
	m.dnsFwd = fwd
}

// ensureDNS binds the tap's DNS shim sockets if the shim is enabled and not yet
// bound. A bind failure is logged and retried on the next reconcile; it never
// blocks or tears down IMDS serving on the same endpoint.
func (m *tapResponderManager) ensureDNS(ctx context.Context, eniID, endpoint string) {
	if m.dnsFwd == nil {
		return
	}
	m.mu.Lock()
	cur, ok := m.active[eniID]
	bound := ok && cur.dnsUDP != nil
	m.mu.Unlock()
	if !ok || bound {
		return
	}

	pc, ln, err := m.dnsListen(ctx, endpoint)
	if err != nil {
		slog.Warn("IMDS: tap DNS shim bind failed, retrying next reconcile", "eni_id", eniID, "endpoint", endpoint, "err", err)
		return
	}

	m.mu.Lock()
	cur, ok = m.active[eniID]
	if !ok || cur.dnsUDP != nil || cur.endpoint != endpoint {
		// The responder was stopped, replaced, or raced us to a bind: this
		// socket pair has no owner to close it later, so drop it now.
		m.mu.Unlock()
		_ = pc.Close()
		_ = ln.Close()
		return
	}
	cur.dnsUDP, cur.dnsTCP = pc, ln
	m.mu.Unlock()

	go m.dnsFwd.serveUDP(pc)
	go m.dnsFwd.serveTCP(ln)
	slog.Info("IMDS: tap DNS shim serving", "eni_id", eniID, "endpoint", endpoint, "addr", pc.LocalAddr().String())
}

// endpointRecreated reports whether eniID's live endpoint device differs from the
// one its responder is bound to — the fingerprint of a stop/start the reconcile
// interval missed, where the endpoint port was torn down and re-added under the
// same ENI-derived name with a fresh ifindex. An ifindex that is unresolvable now
// or was unresolvable at bind (0) is treated as unchanged, so a transient lookup
// miss never churns a healthy responder.
func (m *tapResponderManager) endpointRecreated(cur *activeTapResponder, endpoint string) bool {
	if cur.endpoint != endpoint {
		return true
	}
	if cur.ifindex == 0 {
		return false
	}
	idx, err := m.ifindex(endpoint)
	if err != nil {
		return false
	}
	return idx != cur.ifindex
}

// reconcile converges active responders to the live tap set: starts one for every
// (eniID → endpoint) not yet serving and stops any whose tap has gone. A start
// failure is logged and retried next reconcile so one stalled tap can't block the rest.
func (m *tapResponderManager) reconcile(ctx context.Context, live map[string]string) {
	for eniID, endpoint := range live {
		m.mu.Lock()
		_, suspended := m.missing[eniID]
		m.mu.Unlock()
		if suspended {
			continue
		}
		if err := m.start(ctx, eniID, endpoint); err != nil {
			if errors.Is(err, errENIRecordGone) {
				// The ENI record is gone but its tap lingers. Retrying every pass just
				// churns the log; suspend until the tap leaves the live set.
				m.mu.Lock()
				m.missing[eniID] = struct{}{}
				m.mu.Unlock()
				slog.InfoContext(ctx, "IMDS: ENI record gone; suspending tap responder retries until tap is removed",
					"eni_id", eniID, "endpoint", endpoint)
				continue
			}
			slog.WarnContext(ctx, "IMDS: tap responder start failed during reconcile", "eni_id", eniID, "endpoint", endpoint, "err", err)
		}
	}

	m.mu.Lock()
	var stale []string
	for eniID := range m.active {
		if _, ok := live[eniID]; !ok {
			stale = append(stale, eniID)
		}
	}
	// Clear suspensions whose tap has gone so a re-created ENI retries next pass.
	for eniID := range m.missing {
		if _, ok := live[eniID]; !ok {
			delete(m.missing, eniID)
		}
	}
	m.mu.Unlock()

	for _, eniID := range stale {
		m.stop(eniID)
	}
}

// stop closes the tap's responder. Idempotent; the endpoint port itself is torn
// down separately by RemoveTapDatapath.
func (m *tapResponderManager) stop(eniID string) {
	m.mu.Lock()
	responder := m.active[eniID]
	delete(m.active, eniID)
	m.mu.Unlock()

	if responder == nil {
		return
	}
	responder.closeDNS()
	if err := responder.server.Close(); err != nil {
		slog.Warn("IMDS: tap responder close failed", "eni_id", eniID, "err", err)
	}
	slog.Info("IMDS: tap responder stopped", "eni_id", eniID)
}

// removeIfCurrent deletes eniID from the active set only if it still maps to
// server, so a Serve goroutine exiting unexpectedly cannot evict a newer responder
// that replaced it after a stop/start.
func (m *tapResponderManager) removeIfCurrent(eniID string, server *http.Server) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.active[eniID]; ok && cur.server == server {
		// Close the DNS shim too: a dangling bind would EADDRINUSE the re-start.
		cur.closeDNS()
		delete(m.active, eniID)
	}
}

// shutdown closes every active tap responder.
func (m *tapResponderManager) shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for eniID, responder := range m.active {
		responder.closeDNS()
		if err := responder.server.Close(); err != nil {
			slog.Warn("IMDS: tap responder close failed during shutdown", "eni_id", eniID, "err", err)
		}
		delete(m.active, eniID)
	}
}

// deviceIfindex resolves dev's current kernel ifindex. A torn-down/recreated
// endpoint reappears under the same name with a fresh ifindex, which is how a
// responder bound to the old device (via SO_BINDTODEVICE) is detected as stale.
func deviceIfindex(dev string) (int, error) {
	iface, err := net.InterfaceByName(dev)
	if err != nil {
		return 0, err
	}
	return iface.Index, nil
}

// bindTapListener opens 169.254.169.254:80 on the tap's endpoint via SO_BINDTODEVICE
// — the per-tap serving socket. No netns/CAP_SYS_ADMIN: the endpoint is in the root
// netns on br-imds and owns .254, so the bind targets the address the guest does.
func bindTapListener(ctx context.Context, endpoint string) (net.Listener, error) {
	lc := net.ListenConfig{Control: bindToDeviceControl(endpoint)}
	return lc.Listen(ctx, "tcp4", net.JoinHostPort(MetaDataServerIP, "80"))
}

// bindToDeviceControl returns a ListenConfig.Control that scopes the socket to
// dev via SO_BINDTODEVICE and sets SO_REUSEADDR — the per-tap responder's serving
// socket binds the .254 address but accepts only frames arriving on its endpoint.
func bindToDeviceControl(dev string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var sockErr error
		if err := c.Control(func(fd uintptr) {
			if sockErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, dev); sockErr != nil {
				return
			}
			sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		}); err != nil {
			return err
		}
		return sockErr
	}
}
