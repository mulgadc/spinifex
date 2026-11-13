package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon/mgmtipstore"
)

// Management IP allocator modes, from the daemon's mgmt_ipam setting.
const (
	MgmtIPAMLegacy  = "legacy"
	MgmtIPAMDurable = "durable"
)

// mgmtStoreTimeout bounds one durable record operation, including its retries.
const mgmtStoreTimeout = 10 * time.Second

var errMgmtIPAMUnbound = errors.New("durable mgmt IPAM is not bound")

// SetMode selects the allocator. Durable mode never falls back to the legacy
// record: until BindKV binds the durable bucket, every operation that needs the
// record refuses. An unknown mode is refused and leaves the allocator unchanged.
func (a *MgmtIPAllocator) SetMode(mode string) error {
	switch mode {
	case "", MgmtIPAMLegacy:
		a.mu.Lock()
		a.durable = false
		a.mu.Unlock()
	case MgmtIPAMDurable:
		prefix, err := netip.ParsePrefix(a.subnet)
		if err != nil {
			return fmt.Errorf("mgmt subnet %s: %w", a.subnet, err)
		}
		a.mu.Lock()
		a.durable, a.prefix = true, prefix
		a.mu.Unlock()
	default:
		return fmt.Errorf("unknown mgmt IPAM mode %q", mode)
	}
	return nil
}

// bindDurable binds the durable bucket. A failure is kept and returned by
// every later operation rather than leaving the allocator to guess.
func (a *MgmtIPAllocator) bindDurable(jsManager *JetStreamManager) {
	ctx, cancel := context.WithTimeout(context.Background(), mgmtStoreTimeout)
	defer cancel()
	var store *mgmtipstore.Store
	err := errMgmtIPAMUnbound
	if jsManager != nil && jsManager.js != nil {
		store, err = mgmtipstore.Bind(ctx, jsManager.js, MgmtIPAMBucket)
	}
	if err != nil {
		slog.Error("Durable mgmt IPAM unavailable; system instances get no management address until it binds",
			"bucket", MgmtIPAMBucket, "err", err)
	}
	a.mu.Lock()
	a.store, a.bindErr = store, err
	a.mu.Unlock()
}

func (a *MgmtIPAllocator) durableStore() (*mgmtipstore.Store, netip.Prefix, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		err := a.bindErr
		if err == nil {
			err = errMgmtIPAMUnbound
		}
		return nil, netip.Prefix{}, "", fmt.Errorf("mgmt IP for subnet %s: %w", a.subnet, err)
	}
	return a.store, a.prefix, a.node, nil
}

// withRecord runs op against a freshly loaded snapshot, reloading only when
// op's own conditional write found the record changed. Any other failure,
// including an unknown write outcome, returns at once and is never retried.
func (a *MgmtIPAllocator) withRecord(op func(context.Context, *mgmtipstore.Store, *mgmtipstore.Snapshot, string) error) error {
	store, prefix, node, err := a.durableStore()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), mgmtStoreTimeout)
	defer cancel()
	for range mgmtCASOuterRetries {
		snap, err := store.Load(ctx, prefix)
		if err != nil {
			return err
		}
		err = op(ctx, store, snap, node)
		if !errors.Is(err, mgmtipstore.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("mgmt IP record for %s changed on each of %d attempts: %w", prefix, mgmtCASOuterRetries, mgmtipstore.ErrConflict)
}

// allocateDurable returns the instance's acknowledged address, claiming the
// cached one or inserting a new one. A cached address is re-claimed in the
// record rather than trusted, and another holder of it is refused.
func (a *MgmtIPAllocator) allocateDurable(instanceID string) (string, error) {
	a.mu.Lock()
	cached := a.allocated[instanceID]
	a.mu.Unlock()

	var ip string
	err := a.withRecord(func(ctx context.Context, store *mgmtipstore.Store, snap *mgmtipstore.Snapshot, node string) error {
		want := cached
		for _, e := range snap.Entries() {
			if e.InstanceID == instanceID {
				if cached != "" && e.IP != cached {
					return fmt.Errorf("mgmt IP for %s is recorded as %s but cached as %s", instanceID, e.IP, cached)
				}
				want = e.IP
			}
		}
		if want == "" {
			got, err := store.Insert(ctx, snap, instanceID, node)
			ip = got.IP
			return err
		}
		got, err := store.ClaimAt(ctx, snap, instanceID, node, want)
		ip = got.IP
		return err
	})
	if err != nil {
		return "", fmt.Errorf("allocate mgmt IP for %s: %w", instanceID, err)
	}
	a.mu.Lock()
	a.allocated[instanceID] = ip
	a.mu.Unlock()
	slog.Debug("Management IP allocated", "instance", instanceID, "ip", ip)
	return ip, nil
}

// releaseDurable removes this node's entry for the instance. An empty ip means
// the address was never learned, so any entry the instance holds here goes.
// The cache keeps the address until the removal is acknowledged.
func (a *MgmtIPAllocator) releaseDurable(instanceID, ip string) (removed bool, err error) {
	err = a.withRecord(func(ctx context.Context, store *mgmtipstore.Store, snap *mgmtipstore.Snapshot, node string) error {
		var opErr error
		if ip == "" {
			removed, opErr = store.RemoveUnknown(ctx, snap, instanceID, node)
		} else {
			removed, opErr = store.Remove(ctx, snap, instanceID, node, ip)
		}
		return opErr
	})
	if err != nil {
		return false, fmt.Errorf("release mgmt IP for %s: %w", instanceID, err)
	}
	a.mu.Lock()
	if cur, ok := a.allocated[instanceID]; ok && (ip == "" || cur == ip) {
		delete(a.allocated, instanceID)
	}
	a.mu.Unlock()
	return removed, nil
}

// claimDurable records this node as the holder of the instance's address. It
// refuses an address another instance holds and an entry on another node.
func (a *MgmtIPAllocator) claimDurable(instanceID, mgmtIP string) error {
	err := a.withRecord(func(ctx context.Context, store *mgmtipstore.Store, snap *mgmtipstore.Snapshot, node string) error {
		_, err := store.ClaimAt(ctx, snap, instanceID, node, mgmtIP)
		return err
	})
	if err != nil {
		return fmt.Errorf("claim mgmt IP %s for %s: %w", mgmtIP, instanceID, err)
	}
	a.mu.Lock()
	a.allocated[instanceID] = mgmtIP
	a.mu.Unlock()
	return nil
}

// rebuildDurable claims every local VM's address. A conflict is reported and
// left for the operator: the record is never overwritten to match this node.
func (a *MgmtIPAllocator) rebuildDurable(vms map[string]*vm.VM) {
	for id, v := range vms {
		if v.MgmtIP == "" {
			continue
		}
		if err := a.claimDurable(id, v.MgmtIP); err != nil {
			slog.Error("Local VM's management IP is not reserved for it in the durable record",
				"instance", id, "ip", v.MgmtIP, "err", err)
		}
	}
}
