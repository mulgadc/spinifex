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
type resolveENIFunc func(eniID string) (*eniFacts, error)

// activeTapResponder is one tap's realised serving state: a listener bound to the
// tap's endpoint on br-imds and the http.Server serving it with the tap's ENI.
type activeTapResponder struct {
	listener net.Listener
	server   *http.Server
}

// tapResponderManager runs one IMDS responder per local primary-ENI tap. Each
// responder binds the shared handler to the tap's endpoint on br-imds with
// SO_BINDTODEVICE and serves it with the tap's ENI identity, resolved once at
// start and threaded into every request. Identity is the tap (its endpoint →
// ENI), so no per-request source-IP lookup is needed and overlapping guest CIDRs
// never collide.
type tapResponderManager struct {
	handler http.Handler
	resolve resolveENIFunc
	listen  tapListenFunc

	mu     sync.Mutex
	active map[string]*activeTapResponder // eniID → responder
}

func newTapResponderManager(handler http.Handler, resolve resolveENIFunc, listen tapListenFunc) *tapResponderManager {
	return &tapResponderManager{
		handler: handler,
		resolve: resolve,
		listen:  listen,
		active:  make(map[string]*activeTapResponder),
	}
}

// start resolves the tap's ENI once, binds a listener to its endpoint, and serves
// the shared handler with that identity threaded into every request. Idempotent
// per ENI. A missing ENI record is an error so the caller can retry — the record
// is written before the tap exists, so a miss is a transient ordering gap.
func (m *tapResponderManager) start(ctx context.Context, eniID, endpoint string) error {
	if eniID == "" || endpoint == "" {
		return errors.New("tap responder: eniID and endpoint required")
	}

	m.mu.Lock()
	if _, ok := m.active[eniID]; ok {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	eni, err := m.resolve(eniID)
	if err != nil {
		return fmt.Errorf("resolve eni %s: %w", eniID, err)
	}
	if eni == nil {
		return fmt.Errorf("no ENI record for %s", eniID)
	}

	listener, err := m.listen(ctx, endpoint)
	if err != nil {
		return fmt.Errorf("listen on endpoint %s: %w", endpoint, err)
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
	m.active[eniID] = &activeTapResponder{listener: listener, server: server}
	m.mu.Unlock()

	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			return // clean stop/shutdown already removed the entry
		}
		// Unexpected exit: drop ourselves from the active set so the next reconcile
		// re-starts this tap. Without this the stale entry makes start a no-op and
		// the tap never serves again.
		slog.Error("IMDS: tap responder serve exited", "eni_id", eniID, "endpoint", endpoint, "err", err)
		m.removeIfCurrent(eniID, server)
	}()

	slog.Info("IMDS: tap responder serving", "eni_id", eniID, "endpoint", endpoint, "addr", listener.Addr().String())
	return nil
}

// reconcile converges the active responders to the live tap set: starts a
// responder for every (eniID → endpoint) not yet serving and stops any whose tap
// has gone. start is idempotent, so an already-serving tap is a no-op. A start
// failure (e.g. the ENI record not yet visible, or the endpoint not yet bindable)
// is logged and retried on the next reconcile, not propagated — one stalled tap
// must not block the rest.
func (m *tapResponderManager) reconcile(ctx context.Context, live map[string]string) {
	for eniID, endpoint := range live {
		if err := m.start(ctx, eniID, endpoint); err != nil {
			slog.Warn("IMDS: tap responder start failed during reconcile", "eni_id", eniID, "endpoint", endpoint, "err", err)
		}
	}

	m.mu.Lock()
	var stale []string
	for eniID := range m.active {
		if _, ok := live[eniID]; !ok {
			stale = append(stale, eniID)
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
	if err := responder.server.Close(); err != nil {
		slog.Warn("IMDS: tap responder close failed", "eni_id", eniID, "err", err)
	}
	slog.Info("IMDS: tap responder stopped", "eni_id", eniID)
}

// removeIfCurrent deletes eniID from the active set only if it still maps to
// server. A Serve goroutine that exits unexpectedly calls this so the next
// reconcile re-starts the tap; the identity check avoids evicting a newer
// responder that replaced this one after a stop/start.
func (m *tapResponderManager) removeIfCurrent(eniID string, server *http.Server) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.active[eniID]; ok && cur.server == server {
		delete(m.active, eniID)
	}
}

// shutdown closes every active tap responder.
func (m *tapResponderManager) shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for eniID, responder := range m.active {
		if err := responder.server.Close(); err != nil {
			slog.Warn("IMDS: tap responder close failed during shutdown", "eni_id", eniID, "err", err)
		}
		delete(m.active, eniID)
	}
}

// bindTapListener opens 169.254.169.254:80 bound to the tap's endpoint device via
// SO_BINDTODEVICE — the per-tap serving socket. No netns: the endpoint lives in
// the root netns on br-imds and SO_BINDTODEVICE scopes the listener to it, so no
// setns and no CAP_SYS_ADMIN. The endpoint owns the .254 address (added by the
// host per-tap datapath installer), so the bind targets the address the guest does.
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
