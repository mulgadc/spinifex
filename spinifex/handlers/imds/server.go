package handlers_imds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"golang.org/x/sys/unix"
)

// ensureVethFunc / removeVethFunc are the per-subnet host veth lifecycle hooks,
// injected (not imported) to avoid an import cycle via network/host. vpcd passes
// host.EnsureIMDSVeth / host.RemoveIMDSVeth; tests pass root-free fakes. ensure
// takes the subnet CIDR (added on-link in the netns so the reply resolves the
// guest by ARP) and returns the per-subnet netns the listener must be created in
// plus the host-end veth name (now living inside that netns).
type ensureVethFunc func(ctx context.Context, subnetID string, cidr netip.Prefix) (netnsName, hostEnd string, err error)
type removeVethFunc func(ctx context.Context, subnetID string) error

// listenFunc binds a TCP listener on 169.254.169.254:80 inside the subnet's netns,
// scoped to its host veth. Swappable in tests so bind-manager logic runs without
// CAP_NET_ADMIN or a real netns. An empty netnsName binds in the current netns.
type listenFunc func(ctx context.Context, netnsName, hostEnd string) (net.Listener, error)

// activeBinding is the per-subnet realised state: a listener bound to the subnet's
// host veth and the http.Server serving it.
type activeBinding struct {
	listener net.Listener
	server   *http.Server
}

// bindManager realises the per-chassis IMDS listener stack. It runs on every
// chassis and eagerly materialises a veth + OVS port + listener for every subnet in
// the subnet-veth bucket — lifecycle is "watch the bucket, ensure local state".
type bindManager struct {
	kv         nats.KeyValue // spinifex-network-imds-subnet-veth
	store      *VethStore    // reads the subnet CIDR the host on-link route needs
	handler    http.Handler
	ensureVeth ensureVethFunc
	removeVeth removeVethFunc
	listen     listenFunc

	mu     sync.Mutex
	active map[string]*activeBinding // subnetID → binding
}

func newBindManager(kv nats.KeyValue, handler http.Handler, ensure ensureVethFunc, remove removeVethFunc, listen listenFunc) *bindManager {
	return &bindManager{
		kv:         kv,
		store:      NewVethStore(kv),
		handler:    handler,
		ensureVeth: ensure,
		removeVeth: remove,
		listen:     listen,
		active:     make(map[string]*activeBinding),
	}
}

// sync reads the full subnet-veth bucket and binds a listener for every entry. Run
// once at startup; idempotent, so a re-sync after a transient error is safe.
func (b *bindManager) sync(ctx context.Context) error {
	keys, err := b.kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil
		}
		return fmt.Errorf("list subnet-veth keys: %w", err)
	}
	for _, subnetID := range keys {
		if subnetID == utils.VersionKey {
			continue // schema-version marker written by migrate.RunKV, not a subnet
		}
		vpcID, cidr, err := b.subnetBinding(subnetID)
		if err != nil {
			slog.Error("IMDS: resolve subnet binding during sync", "subnet_id", subnetID, "err", err)
			continue
		}
		if err := b.bind(ctx, subnetID, vpcID, cidr); err != nil {
			slog.Error("IMDS: bind failed during sync", "subnet_id", subnetID, "err", err)
		}
	}
	return nil
}

// subnetBinding reads the persisted veth record for subnetID and returns the two
// facts bind needs: the owning VPC (the subnet→VPC static lookup — the listener
// identifies the subnet, the eni-by-vpc-ip index is keyed vpcID/ip) and the
// subnet CIDR (added on-link in the netns so the reply resolves the guest by
// ARP). A record missing either field is malformed; bind cannot proceed without
// them, and the reconciler re-publishes a complete record to converge.
func (b *bindManager) subnetBinding(subnetID string) (vpcID string, cidr netip.Prefix, err error) {
	rec, err := b.store.Get(subnetID)
	if err != nil {
		return "", netip.Prefix{}, err
	}
	if rec == nil {
		return "", netip.Prefix{}, fmt.Errorf("no veth record for %s", subnetID)
	}
	if rec.VPCID == "" {
		return "", netip.Prefix{}, fmt.Errorf("veth record for %s has no vpc_id", subnetID)
	}
	cidr, err = netip.ParsePrefix(rec.SubnetCIDR)
	if err != nil {
		return "", netip.Prefix{}, fmt.Errorf("parse subnet cidr %q: %w", rec.SubnetCIDR, err)
	}
	return rec.VPCID, cidr, nil
}

// watch reacts to subnet-veth bucket changes for the life of ctx: a Put binds the
// subnet (if not already bound), a Delete unbinds it and tears down the veth. Both
// transitions are idempotent.
func (b *bindManager) watch(ctx context.Context) {
	watcher, err := b.kv.WatchAll(nats.Context(ctx))
	if err != nil {
		slog.Error("IMDS: failed to start subnet-veth watcher", "err", err)
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
				continue // schema-version marker written by migrate.RunKV, not a subnet
			}
			switch entry.Operation() {
			case nats.KeyValuePut:
				vpcID, cidr, err := b.subnetBinding(entry.Key())
				if err != nil {
					slog.Error("IMDS: resolve subnet binding on watch", "subnet_id", entry.Key(), "err", err)
					continue
				}
				if err := b.bind(ctx, entry.Key(), vpcID, cidr); err != nil {
					slog.Error("IMDS: bind failed on watch", "subnet_id", entry.Key(), "err", err)
				}
			case nats.KeyValueDelete, nats.KeyValuePurge:
				b.unbind(ctx, entry.Key())
			}
		}
	}
}

// bind ensures the subnet's host veth (with the subnet CIDR on-link for the reply
// path) then opens a device-scoped listener for subnetID. vpcID is the subnet's
// statically-resolved owning VPC, threaded into every request so the handler can
// key the eni-by-vpc-ip index. A no-op if already bound.
func (b *bindManager) bind(ctx context.Context, subnetID, vpcID string, cidr netip.Prefix) error {
	b.mu.Lock()
	if _, ok := b.active[subnetID]; ok {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	netnsName, hostEnd, err := b.ensureVeth(ctx, subnetID, cidr)
	if err != nil {
		return fmt.Errorf("ensure veth: %w", err)
	}

	listener, err := b.listen(ctx, netnsName, hostEnd)
	if err != nil {
		return fmt.Errorf("listen on %s in netns %s: %w", hostEnd, netnsName, err)
	}

	server := &http.Server{
		Handler:           b.handler,
		ReadHeaderTimeout: 10 * time.Second,
		// Tag every request from this listener with the subnet it serves and that
		// subnet's owning VPC. The handler pairs the VPC with the datapath-attested
		// source IP to resolve the ENI; the subnet ID rides along for triage.
		BaseContext: func(net.Listener) context.Context {
			c := context.WithValue(ctx, ctxKeyVPCID, vpcID)
			return context.WithValue(c, ctxKeySubnetID, subnetID)
		},
	}

	b.mu.Lock()
	// Re-check under lock in case a concurrent bind won the race.
	if _, ok := b.active[subnetID]; ok {
		b.mu.Unlock()
		_ = listener.Close()
		return nil
	}
	b.active[subnetID] = &activeBinding{listener: listener, server: server}
	b.mu.Unlock()

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("IMDS: listener serve exited", "subnet_id", subnetID, "host_end", hostEnd, "err", err)
		}
	}()

	slog.Info("IMDS: listener bound", "subnet_id", subnetID, "vpc_id", vpcID, "netns", netnsName, "host_end", hostEnd, "addr", listener.Addr().String())
	return nil
}

// unbind closes the subnet's listener and removes its host veth. Idempotent.
func (b *bindManager) unbind(ctx context.Context, subnetID string) {
	b.mu.Lock()
	binding := b.active[subnetID]
	delete(b.active, subnetID)
	b.mu.Unlock()

	if binding != nil {
		if err := binding.server.Close(); err != nil {
			slog.Warn("IMDS: listener close failed", "subnet_id", subnetID, "err", err)
		}
	}
	if err := b.removeVeth(ctx, subnetID); err != nil {
		slog.Warn("IMDS: veth removal failed", "subnet_id", subnetID, "err", err)
	}
	slog.Info("IMDS: listener unbound", "subnet_id", subnetID)
}

// shutdown closes every active listener. The host veths are intentionally left
// in place: they are cheap, idempotent to re-ensure on the next start, and
// tearing them down on a clean vpcd restart would needlessly disrupt any other
// chassis-local consumer.
func (b *bindManager) shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for subnetID, binding := range b.active {
		if err := binding.server.Close(); err != nil {
			slog.Warn("IMDS: listener close failed during shutdown", "subnet_id", subnetID, "err", err)
		}
		delete(b.active, subnetID)
	}
}

// bindLocalListener opens a TCP listener on 169.254.169.254:80 inside the subnet's
// netns, where the host-end veth carries 169.254.169.254/30 with the subnet CIDR
// on-link — so the host-served reply resolves the guest by ARP over the localport
// on the guest's own L2 switch (SO_BINDTODEVICE alone in the root netns could not
// route the SYN-ACK). The netns also isolates subnets whose CIDRs overlap, since
// each is its own routing domain. The socket must be *created* in the netns, so we
// enter it on a locked thread for the socket call and restore the root netns before
// serving.
// SO_BINDTODEVICE (belt-and-braces — there is only one veth in the netns) and
// SO_REUSEADDR (fast rebind on vpcd restart) are still set on the fd.
func bindLocalListener(ctx context.Context, netnsName, hostEnd string) (net.Listener, error) {
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

	bind := func() (net.Listener, error) {
		return lc.Listen(ctx, "tcp4", net.JoinHostPort(MetaDataServerIP, "80"))
	}

	if netnsName == "" {
		// No netns (tests, or a degraded host): bind in the current netns.
		return bind()
	}

	var ln net.Listener
	err := inNetns(netnsName, func() error {
		var e error
		ln, e = bind()
		return e
	})
	if err != nil {
		return nil, err
	}
	return ln, nil
}

// inNetns runs fn on a dedicated OS thread switched into the named network
// namespace, then restores the root netns. fn's socket-creating syscalls happen
// in the target netns, so the listener fd belongs to it; once created, the fd is
// served normally from any thread. If the root netns cannot be restored the
// thread is poisoned, so the goroutine returns while still locked and the Go
// runtime retires the thread rather than returning it to the pool.
func inNetns(netnsName string, fn func() error) error {
	const netnsDir = "/var/run/netns/"
	errCh := make(chan error, 1)

	go func() {
		runtime.LockOSThread()

		orig, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			runtime.UnlockOSThread()
			errCh <- fmt.Errorf("open current netns: %w", err)
			return
		}
		defer func() { _ = orig.Close() }()

		target, err := os.Open(netnsDir + netnsName)
		if err != nil {
			runtime.UnlockOSThread()
			errCh <- fmt.Errorf("open netns %s: %w", netnsName, err)
			return
		}
		defer func() { _ = target.Close() }()

		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			errCh <- fmt.Errorf("setns into %s: %w", netnsName, err)
			return
		}

		fnErr := fn()

		if err := unix.Setns(int(orig.Fd()), unix.CLONE_NEWNET); err != nil {
			// Cannot return to the root netns — leave the thread locked so the
			// runtime destroys it when this goroutine exits.
			errCh <- errors.Join(fnErr, fmt.Errorf("restore root netns: %w", err))
			return
		}
		runtime.UnlockOSThread()
		errCh <- fnErr
	}()

	return <-errCh
}
