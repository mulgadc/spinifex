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

var (
	errMgmtIPAMUnbound   = errors.New("durable mgmt IPAM is not bound")
	errMgmtIPNotReserved = errors.New("mgmt IP is not reserved in the durable record")

	// errMgmtIPUnresolvedHolder refuses new addresses while a local guest's
	// address is unacknowledged by the durable record.
	errMgmtIPUnresolvedHolder = errors.New("a local mgmt IP holder is not reserved in the durable record")
)

func entryFor(snap *mgmtipstore.Snapshot, instanceID string) (mgmtipstore.Entry, bool) {
	for _, e := range snap.Entries() {
		if e.InstanceID == instanceID {
			return e, true
		}
	}
	return mgmtipstore.Entry{}, false
}

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

// bindDurable binds the durable bucket, never creating it. Until a bind has
// succeeded each operation tries again; once bound, the store is kept and its
// own checks refuse a bucket that was recreated or changed.
func (a *MgmtIPAllocator) bindDurable() (*mgmtipstore.Store, error) {
	a.mu.Lock()
	store, jsManager := a.store, a.jsManager
	a.mu.Unlock()
	if store != nil {
		return store, nil
	}
	if jsManager == nil || jsManager.js == nil {
		return nil, errMgmtIPAMUnbound
	}
	ctx, cancel := context.WithTimeout(context.Background(), mgmtStoreTimeout)
	defer cancel()
	bound, err := mgmtipstore.Bind(ctx, jsManager.js, MgmtIPAMBucket)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store == nil {
		a.store = bound
	}
	return a.store, nil
}

func (a *MgmtIPAllocator) durableStore() (*mgmtipstore.Store, netip.Prefix, string, error) {
	store, err := a.bindDurable()
	if err != nil {
		return nil, netip.Prefix{}, "", fmt.Errorf("mgmt IP for subnet %s: %w", a.subnet, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return store, a.prefix, a.node, nil
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

// allocateDurable returns the instance's acknowledged address and whether this
// call created its reservation. A recorded entry is re-acknowledged and a new
// instance gets a new address; a cached address the record lacks is refused.
func (a *MgmtIPAllocator) allocateDurable(instanceID string) (ip string, inserted bool, err error) {
	a.mu.Lock()
	cached := a.allocated[instanceID]
	a.mu.Unlock()
	if cached == "" {
		if err := a.resolveLocalHolders(); err != nil {
			return "", false, fmt.Errorf("allocate mgmt IP for %s: %w", instanceID, err)
		}
	}

	err = a.withRecord(func(ctx context.Context, store *mgmtipstore.Store, snap *mgmtipstore.Snapshot, node string) error {
		inserted = false
		recorded, ok := entryFor(snap, instanceID)
		switch {
		case ok && cached != "" && recorded.IP != cached:
			return fmt.Errorf("mgmt IP for %s is recorded as %s but cached as %s", instanceID, recorded.IP, cached)
		case ok:
			got, err := store.ClaimAt(ctx, snap, instanceID, node, recorded.IP)
			ip = got.IP
			return err
		case cached != "":
			return fmt.Errorf("%w: %s at %s", errMgmtIPNotReserved, instanceID, cached)
		}
		got, err := store.Insert(ctx, snap, instanceID, node)
		ip, inserted = got.IP, err == nil
		return err
	})
	if err != nil {
		return "", false, fmt.Errorf("allocate mgmt IP for %s: %w", instanceID, err)
	}
	a.mu.Lock()
	a.allocated[instanceID] = ip
	a.confirmed[instanceID] = ip
	a.mu.Unlock()
	slog.Debug("Management IP allocated", "instance", instanceID, "ip", ip)
	return ip, inserted, nil
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
		delete(a.confirmed, instanceID)
	}
	a.mu.Unlock()
	return removed, nil
}

// confirmDurable re-acknowledges this node's existing reservation of mgmtIP
// for the instance. A missing reservation is refused, not recreated: another
// node may have handed the address out while it was missing.
func (a *MgmtIPAllocator) confirmDurable(instanceID, mgmtIP string) error {
	err := a.withRecord(func(ctx context.Context, store *mgmtipstore.Store, snap *mgmtipstore.Snapshot, node string) error {
		if _, ok := entryFor(snap, instanceID); !ok {
			return fmt.Errorf("%w: %s at %s", errMgmtIPNotReserved, instanceID, mgmtIP)
		}
		_, err := store.ClaimAt(ctx, snap, instanceID, node, mgmtIP)
		return err
	})
	if err != nil {
		return fmt.Errorf("claim mgmt IP %s for %s: %w", mgmtIP, instanceID, err)
	}
	a.mu.Lock()
	a.allocated[instanceID] = mgmtIP
	a.confirmed[instanceID] = mgmtIP
	a.mu.Unlock()
	return nil
}

// claimDurable reserves mgmtIP for the instance on this node, creating the
// entry when the address is free; an address or instance held elsewhere is
// refused. Callers have proven the instance's previous launch stopped.
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
	a.confirmed[instanceID] = mgmtIP
	a.mu.Unlock()
	return nil
}

// mgmtLaunchAddresses is the durable record as the VM manager's address
// authority for launches it has proven safe.
type mgmtLaunchAddresses struct{ a *MgmtIPAllocator }

var _ vm.MgmtAddresses = mgmtLaunchAddresses{}

func (m mgmtLaunchAddresses) Claim(instanceID, ip string) error {
	return m.a.claimDurable(instanceID, ip)
}

func (m mgmtLaunchAddresses) Confirm(instanceID, ip string) error {
	return m.a.confirmDurable(instanceID, ip)
}

func (m mgmtLaunchAddresses) Release(instanceID, ip string) error {
	_, err := m.a.releaseDurable(instanceID, ip)
	return err
}

// rebuildDurable confirms every local VM's reservation. A missing or
// conflicting one is reported, never reconstructed, and stays unconfirmed so
// it blocks new allocations (see resolveLocalHolders).
func (a *MgmtIPAllocator) rebuildDurable(vms map[string]*vm.VM) {
	for id, v := range vms {
		if v.MgmtIP == "" {
			continue
		}
		if err := a.confirmDurable(id, v.MgmtIP); err != nil {
			slog.Error("Local VM's management IP is not reserved for it in the durable record",
				"instance", id, "ip", v.MgmtIP, "err", err)
		}
	}
}

// resolveLocalHolders confirms every address this node holds that the record
// has not yet acknowledged. A new address is handed out only when none is left
// unconfirmed: the record may lack a live guest's address and offer it again.
func (a *MgmtIPAllocator) resolveLocalHolders() error {
	a.mu.Lock()
	pending := make(map[string]string)
	for id, ip := range a.allocated {
		if a.confirmed[id] != ip {
			pending[id] = ip
		}
	}
	a.mu.Unlock()

	var errs []error
	for id, ip := range pending {
		if err := a.confirmDurable(id, ip); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", errMgmtIPUnresolvedHolder, errors.Join(errs...))
	}
	return nil
}
