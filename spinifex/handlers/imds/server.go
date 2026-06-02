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

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"golang.org/x/sys/unix"
)

// ensureVethFunc / removeVethFunc are the per-VPC host veth lifecycle hooks. They
// are injected (not imported) because network/host transitively imports
// network/topology, which imports this package — importing host back would close
// a cycle. The vpcd wiring (which may import host) passes host.EnsureIMDSVeth /
// host.RemoveIMDSVeth; tests pass fakes that need no root, OVS, or interfaces.
type ensureVethFunc func(ctx context.Context, vpcID string) (hostEnd string, err error)
type removeVethFunc func(ctx context.Context, vpcID string) error

// listenFunc binds a TCP listener on 169.254.169.254:80 scoped to a host veth.
// Swappable in tests so bind-manager logic runs without CAP_NET_RAW or the
// link-local address actually being present.
type listenFunc func(ctx context.Context, hostEnd string) (net.Listener, error)

// activeBinding is the per-VPC realised state: a listener bound to the VPC's
// host veth and the http.Server serving it.
type activeBinding struct {
	listener net.Listener
	server   *http.Server
}

// bindManager realises the per-chassis IMDS listener stack. It runs on every
// chassis (load-bearing for the localport design) and eagerly materialises a
// veth + OVS port + listener for every VPC entry in the vpc-veth bucket, not
// only VPCs whose VMs currently run on this chassis. The lifecycle is purely
// "watch the bucket, ensure local state" — no per-VM signalling.
type bindManager struct {
	kv         nats.KeyValue // spinifex-network-imds-vpc-veth
	handler    http.Handler
	ensureVeth ensureVethFunc
	removeVeth removeVethFunc
	listen     listenFunc

	mu     sync.Mutex
	active map[string]*activeBinding // vpcID → binding
}

func newBindManager(kv nats.KeyValue, handler http.Handler, ensure ensureVethFunc, remove removeVethFunc, listen listenFunc) *bindManager {
	return &bindManager{
		kv:         kv,
		handler:    handler,
		ensureVeth: ensure,
		removeVeth: remove,
		listen:     listen,
		active:     make(map[string]*activeBinding),
	}
}

// sync reads the full vpc-veth bucket and binds a listener for every entry. Run
// once at startup; idempotent, so a re-sync after a transient error is safe.
func (b *bindManager) sync(ctx context.Context) error {
	keys, err := b.kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list vpc-veth keys: %w", err)
	}
	for _, vpcID := range keys {
		if vpcID == utils.VersionKey {
			continue // schema-version marker written by migrate.RunKV, not a VPC
		}
		if err := b.bind(ctx, vpcID); err != nil {
			slog.Error("IMDS: bind failed during sync", "vpc_id", vpcID, "err", err)
		}
	}
	return nil
}

// watch reacts to vpc-veth bucket changes for the life of ctx: a Put binds the
// VPC (if not already bound), a Delete unbinds it and tears down the veth. Both
// transitions are idempotent.
func (b *bindManager) watch(ctx context.Context) {
	watcher, err := b.kv.WatchAll(nats.Context(ctx))
	if err != nil {
		slog.Error("IMDS: failed to start vpc-veth watcher", "err", err)
		return
	}
	defer func() { _ = watcher.Stop() }()

	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-watcher.Updates():
			if !ok {
				return
			}
			if entry == nil {
				// nil marks the end of the initial replay; sync() already
				// covered those, so there is nothing extra to do.
				continue
			}
			if entry.Key() == utils.VersionKey {
				continue // schema-version marker written by migrate.RunKV, not a VPC
			}
			switch entry.Operation() {
			case nats.KeyValuePut:
				if err := b.bind(ctx, entry.Key()); err != nil {
					slog.Error("IMDS: bind failed on watch", "vpc_id", entry.Key(), "err", err)
				}
			case nats.KeyValueDelete, nats.KeyValuePurge:
				b.unbind(ctx, entry.Key())
			}
		}
	}
}

// bind ensures the host veth then opens a device-scoped listener for vpcID. A
// no-op if the VPC is already bound.
func (b *bindManager) bind(ctx context.Context, vpcID string) error {
	b.mu.Lock()
	if _, ok := b.active[vpcID]; ok {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	hostEnd, err := b.ensureVeth(ctx, vpcID)
	if err != nil {
		return fmt.Errorf("ensure veth: %w", err)
	}

	listener, err := b.listen(ctx, hostEnd)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", hostEnd, err)
	}

	server := &http.Server{
		Handler:           b.handler,
		ReadHeaderTimeout: 10 * time.Second,
		// Tag every request from this listener with its VPC ID. The handler
		// pairs it with the datapath-attested source IP to resolve the ENI.
		BaseContext: func(net.Listener) context.Context {
			return context.WithValue(ctx, ctxKeyVPCID, vpcID)
		},
	}

	b.mu.Lock()
	// Re-check under lock in case a concurrent bind won the race.
	if _, ok := b.active[vpcID]; ok {
		b.mu.Unlock()
		_ = listener.Close()
		return nil
	}
	b.active[vpcID] = &activeBinding{listener: listener, server: server}
	b.mu.Unlock()

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("IMDS: listener serve exited", "vpc_id", vpcID, "host_end", hostEnd, "err", err)
		}
	}()

	slog.Info("IMDS: listener bound", "vpc_id", vpcID, "host_end", hostEnd)
	return nil
}

// unbind closes the VPC's listener and removes its host veth. Idempotent.
func (b *bindManager) unbind(ctx context.Context, vpcID string) {
	b.mu.Lock()
	binding := b.active[vpcID]
	delete(b.active, vpcID)
	b.mu.Unlock()

	if binding != nil {
		if err := binding.server.Close(); err != nil {
			slog.Warn("IMDS: listener close failed", "vpc_id", vpcID, "err", err)
		}
	}
	if err := b.removeVeth(ctx, vpcID); err != nil {
		slog.Warn("IMDS: veth removal failed", "vpc_id", vpcID, "err", err)
	}
	slog.Info("IMDS: listener unbound", "vpc_id", vpcID)
}

// shutdown closes every active listener. The host veths are intentionally left
// in place: they are cheap, idempotent to re-ensure on the next start, and
// tearing them down on a clean vpcd restart would needlessly disrupt any other
// chassis-local consumer.
func (b *bindManager) shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for vpcID, binding := range b.active {
		if err := binding.server.Close(); err != nil {
			slog.Warn("IMDS: listener close failed during shutdown", "vpc_id", vpcID, "err", err)
		}
		delete(b.active, vpcID)
	}
}

// bindLocalListener opens a TCP listener on 169.254.169.254:80 pinned to a host
// veth via SO_BINDTODEVICE. That pinning does the two things the design relies
// on: it scopes receipt to the one veth (so VPC A's request never matches VPC
// B's listener despite the shared IP:port) and forces replies back out the same
// veth regardless of the host routing table. SO_REUSEADDR lets a restarting
// vpcd rebind before the old socket's TIME_WAIT drains.
func bindLocalListener(ctx context.Context, hostEnd string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var sockErr error
			if err := c.Control(func(fd uintptr) {
				if sockErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, hostEnd); sockErr != nil {
					return
				}
				sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			}); err != nil {
				return err
			}
			return sockErr
		},
	}
	return lc.Listen(ctx, "tcp4", net.JoinHostPort(MetaDataServerIP, "80"))
}
