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

// ensureVethFunc is the per-subnet host veth lifecycle hook injected to avoid an import cycle.
// Returns the per-subnet netns name and the host-end veth name.
type ensureVethFunc func(ctx context.Context, subnetID string, cidr netip.Prefix) (netnsName, hostEnd string, err error)
type removeVethFunc func(ctx context.Context, subnetID string) error

// listenFunc binds a TCP listener on 169.254.169.254:80 inside the subnet's netns.
// An empty netnsName binds in the current netns; swapped in tests to avoid CAP_NET_ADMIN.
type listenFunc func(ctx context.Context, netnsName, hostEnd string) (net.Listener, error)

// activeBinding is the per-subnet realised state: a listener bound to the subnet's
// host veth and the http.Server serving it.
type activeBinding struct {
	listener net.Listener
	server   *http.Server
}

// bindManager realises the per-chassis IMDS listener stack, materialising a listener
// per subnet by watching the subnet-veth bucket.
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

// sync reads the subnet-veth bucket and binds a listener for every entry.
// Idempotent; safe to re-call after a transient error.
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
			continue // schema-version marker, not a subnet
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

// subnetBinding returns the owning VPC ID and subnet CIDR for the given subnetID.
// Returns an error if the veth record is missing or incomplete.
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

// watch reacts to subnet-veth bucket changes: Put binds, Delete/Purge unbinds. Idempotent.
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
				continue // end of initial replay; sync() already handled those
			}
			if entry.Key() == utils.VersionKey {
				continue // schema-version marker, not a subnet
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

// bind ensures the subnet's host veth, then opens a device-scoped listener.
// vpcID is threaded into every request for ENI resolution. No-op if already bound.
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
		BaseContext: func(net.Listener) context.Context {
			c := context.WithValue(ctx, ctxKeyVPCID, vpcID)
			return context.WithValue(c, ctxKeySubnetID, subnetID)
		},
	}

	b.mu.Lock()
	if _, ok := b.active[subnetID]; ok { // re-check under lock
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

// shutdown closes every active listener. Host veths are left in place for cheap re-ensure on restart.
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

// bindToDeviceControl returns a ListenConfig.Control that scopes the socket to
// dev via SO_BINDTODEVICE and sets SO_REUSEADDR. Shared by the per-subnet
// localport listener and the per-tap responder.
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

// bindLocalListener opens 169.254.169.254:80 inside the subnet's netns.
// The socket is created inside the netns (locked thread) so the fd belongs to it;
// SO_BINDTODEVICE and SO_REUSEADDR are also set.
func bindLocalListener(ctx context.Context, netnsName, hostEnd string) (net.Listener, error) {
	lc := net.ListenConfig{Control: bindToDeviceControl(hostEnd)}

	bind := func() (net.Listener, error) {
		return lc.Listen(ctx, "tcp4", net.JoinHostPort(MetaDataServerIP, "80"))
	}

	if netnsName == "" {
		return bind() // no netns: bind in current netns (tests or degraded host)
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

// inNetns runs fn on a locked OS thread switched into the named netns, then restores root netns.
// If root netns cannot be restored the goroutine exits locked so the runtime retires the thread.
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
			// Thread poisoned — leave it locked so the runtime retires it.
			errCh <- errors.Join(fnErr, fmt.Errorf("restore root netns: %w", err))
			return
		}
		runtime.UnlockOSThread()
		errCh <- fnErr
	}()

	return <-errCh
}
