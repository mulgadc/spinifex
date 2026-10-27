package floating

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// purposeEIP duplicates domains/ec2/vpc.PurposeEIP so the allocator can tell
// an Elastic IP from an auto-assigned address without importing handlers.
const purposeEIP = "eip"

// Binding is one allocated address and the AWS identity that owns it, keyed in
// the record by the address.
type Binding struct {
	AddrID       string `json:"addr_id"`
	Purpose      string `json:"purpose,omitempty"`
	AllocationID string `json:"allocation_id,omitempty"`
	ENIID        string `json:"eni_id,omitempty"`
	InstanceID   string `json:"instance_id,omitempty"`
	// GatewayVPCID names the VPC whose NAT gateway answers on the address. Such
	// an address has no ENI, so this is what says which node must hold it.
	GatewayVPCID string `json:"gateway_vpc_id,omitempty"`
	// Holder is the provider host this node last confirmed the address on. It
	// is observed state, written after the provider agreed, never identity.
	Holder string `json:"holder,omitempty"`
}

// owned reports whether anything should be answering on the address.
func (b Binding) owned() bool { return b.ENIID != "" || b.GatewayVPCID != "" }

// Record is the per-pool binding set.
type Record struct {
	Bindings map[string]Binding `json:"bindings"`
}

// Store persists bindings. JetStream KV in store.go; an interface so the
// allocator's tests need no NATS.
type Store interface {
	Mutate(ctx context.Context, poolName string, fn func(*Record) (bool, error)) error
	Get(ctx context.Context, poolName string) (Record, error)
}

// Config wires one pool to this node.
type Config struct {
	Pool external.ExternalPoolConfig
	// HostID is this node's provider host ID, which the holder pass attaches to.
	HostID string
	// LocalPorts lists the OVS ports plugged on this node, keyed by port name.
	LocalPorts func(ctx context.Context) (map[string]struct{}, error)
	// LocalGateway reports whether vpcID's gateway chassis is this node.
	LocalGateway func(ctx context.Context, vpcID string) (bool, error)
}

// PoolAllocator implements external.Allocator over a floating Provider.
type PoolAllocator struct {
	provider Provider
	store    Store
	cfg      Config
	// suspects holds unbound labelled addresses one reconcile pass has seen,
	// so the next can collect them. See Reconcile.
	suspects map[string]struct{}
}

var (
	_ external.Allocator      = (*PoolAllocator)(nil)
	_ external.OwnerTracker   = (*PoolAllocator)(nil)
	_ external.AddressClaimer = (*PoolAllocator)(nil)
)

// New constructs an allocator bound to a single pool.
func New(provider Provider, store Store, cfg Config) (*PoolAllocator, error) {
	if provider == nil {
		return nil, errors.New("floating: nil provider")
	}
	if store == nil {
		return nil, errors.New("floating: nil store")
	}
	if cfg.HostID == "" {
		return nil, fmt.Errorf("floating: pool %q missing provider host id", cfg.Pool.Name)
	}
	if cfg.Pool.LabelPrefix == "" {
		return nil, fmt.Errorf("floating: pool %q missing label prefix", cfg.Pool.Name)
	}
	return &PoolAllocator{provider: provider, store: store, cfg: cfg, suspects: map[string]struct{}{}}, nil
}

// Pool returns the pool this allocator manages.
func (a *PoolAllocator) Pool() external.ExternalPoolConfig { return a.cfg.Pool }

// Allocate creates a labelled address and records it, unattached.
//
// The address is created before the record is written. A crash in between
// leaves a labelled address with no binding, which Reconcile collects; the
// reverse order could leave a binding naming an address that never existed.
func (a *PoolAllocator) Allocate(ctx context.Context, req external.AllocateRequest) (netip.Addr, error) {
	if req.PoolName != "" && req.PoolName != a.cfg.Pool.Name {
		return netip.Addr{}, fmt.Errorf("floating: pool mismatch (got %q, bound to %q)", req.PoolName, a.cfg.Pool.Name)
	}

	addr, err := a.provider.Create(ctx, a.label(req))
	if err != nil {
		return netip.Addr{}, capacityErr(req.Purpose, fmt.Errorf("floating: create address: %w", err), err)
	}
	if !addr.IP.IsValid() {
		a.undo(ctx, addr)
		return netip.Addr{}, fmt.Errorf("floating: provider returned address %s with no IP", addr.ID)
	}

	key := addr.IP.String()
	binding := Binding{
		AddrID:       addr.ID,
		Purpose:      req.Purpose,
		AllocationID: req.AllocationID,
		ENIID:        req.ENIID,
		InstanceID:   req.InstanceID,
	}
	err = a.store.Mutate(ctx, a.cfg.Pool.Name, func(rec *Record) (bool, error) {
		rec.Bindings[key] = binding
		return true, nil
	})
	if err != nil {
		a.undo(ctx, addr)
		return netip.Addr{}, fmt.Errorf("floating: record binding for %s: %w", key, err)
	}

	slog.InfoContext(ctx, "floating allocated external IP",
		"pool", a.cfg.Pool.Name, "public_ip", key, "addr_id", addr.ID,
		"purpose", req.Purpose, "eni_id", req.ENIID)
	return addr.IP, nil
}

// Release drops the binding and deletes the address. ownerENIID scopes the
// release as StaticPoolAllocator.Release does, so a teardown sweep naming a
// stale owner is a no-op rather than a double-free.
//
// The record is dropped first: if the delete then fails, the address is an
// unbound labelled address that Reconcile collects, not a record that lies.
func (a *PoolAllocator) Release(ctx context.Context, poolName string, ip netip.Addr, ownerENIID string) error {
	if poolName != "" && poolName != a.cfg.Pool.Name {
		return fmt.Errorf("floating: pool mismatch (got %q, bound to %q)", poolName, a.cfg.Pool.Name)
	}
	if !ip.IsValid() {
		return errors.New("floating: invalid ip")
	}
	key := ip.String()

	var released Binding
	var found bool
	err := a.store.Mutate(ctx, a.cfg.Pool.Name, func(rec *Record) (bool, error) {
		released, found = Binding{}, false

		b, ok := rec.Bindings[key]
		if !ok {
			if ownerENIID != "" {
				return false, nil
			}
			return false, fmt.Errorf("floating: %s not allocated in pool %s", key, a.cfg.Pool.Name)
		}
		if ownerENIID != "" && b.ENIID != "" && b.ENIID != ownerENIID {
			slog.InfoContext(ctx, "floating release skip — address reassigned to a different ENI (stale release)",
				"pool", a.cfg.Pool.Name, "public_ip", key,
				"stale_owner_eni", ownerENIID, "current_owner_eni", b.ENIID)
			return false, nil
		}
		// Only ReleaseAddress, which names no interface, may free an Elastic IP.
		// Instance teardown must leave it allocated, as AWS does.
		if ownerENIID != "" && (b.Purpose == purposeEIP || b.AllocationID != "") {
			slog.InfoContext(ctx, "floating release skip — address belongs to an Elastic IP allocation",
				"pool", a.cfg.Pool.Name, "public_ip", key,
				"allocation_id", b.AllocationID, "owner_eni", ownerENIID)
			return false, nil
		}
		delete(rec.Bindings, key)
		released, found = b, true
		return true, nil
	})
	if err != nil || !found {
		return err
	}

	if err := a.destroy(ctx, released.AddrID); err != nil {
		return fmt.Errorf("floating: delete address %s: %w", released.AddrID, err)
	}
	slog.InfoContext(ctx, "floating released external IP",
		"pool", a.cfg.Pool.Name, "public_ip", key, "addr_id", released.AddrID)
	return nil
}

// BindOwner records which ENI an allocated address is associated with. An
// Elastic IP is allocated bare and associated later, and without this the
// holder pass could not tell which node should hold it. An empty eniID clears
// the owner, which is what a disassociate leaves.
func (a *PoolAllocator) BindOwner(ctx context.Context, poolName string, ip netip.Addr, eniID string) error {
	if poolName != "" && poolName != a.cfg.Pool.Name {
		return fmt.Errorf("floating: pool mismatch (got %q, bound to %q)", poolName, a.cfg.Pool.Name)
	}
	if !ip.IsValid() {
		return errors.New("floating: invalid ip")
	}
	key := ip.String()
	return a.store.Mutate(ctx, a.cfg.Pool.Name, func(rec *Record) (bool, error) {
		b, ok := rec.Bindings[key]
		if !ok || b.ENIID == eniID {
			return false, nil
		}
		b.ENIID = eniID
		rec.Bindings[key] = b
		return true, nil
	})
}

// BindingFor returns the binding behind an allocated address.
func (a *PoolAllocator) BindingFor(ctx context.Context, ip netip.Addr) (Binding, bool, error) {
	rec, err := a.store.Get(ctx, a.cfg.Pool.Name)
	if errors.Is(err, kvstore.ErrNotFound) {
		return Binding{}, false, nil
	}
	if err != nil {
		return Binding{}, false, err
	}
	b, ok := rec.Bindings[ip.String()]
	return b, ok, nil
}

// undo deletes an address a failed Allocate made. Failures are logged, not
// returned: the caller is already failing, and the label marks it for Reconcile.
func (a *PoolAllocator) undo(ctx context.Context, addr Address) {
	if err := a.destroy(ctx, addr.ID); err != nil {
		slog.WarnContext(ctx, "floating: could not roll back address; reconcile will collect it",
			"addr_id", addr.ID, "public_ip", addr.IP.String(), "error", err)
	}
}

// destroy detaches and then deletes an address; a missing one is already gone.
// Vultr documents delete as detaching implicitly, but has been measured to
// refuse it with "Unable to detach subnet" while the address is attached.
func (a *PoolAllocator) destroy(ctx context.Context, addrID string) error {
	if err := a.provider.Detach(ctx, addrID); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("detach: %w", err)
	}
	if err := a.provider.Delete(ctx, addrID); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// labelPrefix starts the label of every address this deployment makes.
func (a *PoolAllocator) labelPrefix() string { return a.cfg.Pool.LabelPrefix + ":" }

// label is the provider label for an allocation: the deployment prefix plus
// the AWS identity, so the provider's console shows what claimed the address.
// The holder is not in it, since that changes on every move.
func (a *PoolAllocator) label(req external.AllocateRequest) string {
	id := "unclaimed"
	switch {
	case req.AllocationID != "":
		id = req.AllocationID
	case req.ENIID != "":
		id = req.ENIID
	case req.InstanceID != "":
		id = req.InstanceID
	}
	return a.labelPrefix() + id
}

// ours reports whether an address carries this deployment's label.
func (a *PoolAllocator) ours(addr Address) bool {
	return strings.HasPrefix(addr.Label, a.labelPrefix())
}

// capacityErr maps the provider quota onto the AWS code for the call. An
// Elastic IP allocation that hits a limit is AddressLimitExceeded in AWS; an
// auto-assigned address has no such limit there, so it reports capacity.
func capacityErr(purpose string, wrapped, raw error) error {
	if !errors.Is(raw, ErrQuotaExceeded) {
		return wrapped
	}
	code := awserrors.ErrorInsufficientAddressCapacity
	if purpose == purposeEIP {
		code = awserrors.ErrorAddressLimitExceeded
	}
	return fmt.Errorf("%w: %w", errors.New(code), wrapped)
}
