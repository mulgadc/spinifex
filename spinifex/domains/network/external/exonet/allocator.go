// Package exonet implements external.Allocator against Exoscale Elastic IPs.
//
// A manual Exoscale EIP is delivered to the instance addressed to itself, and
// the instance may use it as a source, so the address Spinifex hands out is the
// address on the wire. That makes this the OCI allocator without its hardest
// half: no private-IP object, no VNIC and no public-to-private mapping. What
// remains is the part every cloud shares — the address only exists once the
// provider has been asked for it, so allocation is an API round trip.
//
// One node, one Exoscale instance: Allocate creates the EIP and attaches it to
// this node straight away, and the routed EIP path takes it from there exactly
// as it does on bare metal.
package exonet

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
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/exoscale"
)

// MarkerPrefix starts the description of every EIP this package creates.
// Exoscale EIPs carry no labels, so the description is the only place to say
// who made one, and Reconcile never touches an EIP without our marker.
const MarkerPrefix = "spinifex:"

// purposeEIP duplicates domains/ec2/vpc.PurposeEIP so the allocator can tell
// an Elastic IP from an auto-assigned address without importing handlers.
const purposeEIP = "eip"

// Binding is one allocated EIP and the AWS identity that claimed it, keyed in
// the record by the address.
type Binding struct {
	EIPID        string `json:"eip_id"`
	Purpose      string `json:"purpose,omitempty"`
	AllocationID string `json:"allocation_id,omitempty"`
	ENIID        string `json:"eni_id,omitempty"`
	InstanceID   string `json:"instance_id,omitempty"`
	// NodeInstanceID is the Exoscale instance the EIP is attached to. With one
	// node it is always ours; recording it now means a second node adds a
	// check rather than a migration.
	NodeInstanceID string `json:"node_instance_id"`
}

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

// Config wires one pool to one Exoscale instance.
type Config struct {
	Pool external.ExternalPoolConfig
	// InstanceID is this node's Exoscale instance, which every EIP attaches to.
	InstanceID string
}

// PoolAllocator implements external.Allocator against Exoscale.
type PoolAllocator struct {
	client exoscale.Client
	store  Store
	cfg    Config
}

var _ external.Allocator = (*PoolAllocator)(nil)

// New constructs an allocator bound to a single pool.
func New(client exoscale.Client, store Store, cfg Config) (*PoolAllocator, error) {
	if client == nil {
		return nil, errors.New("exonet: nil Exoscale client")
	}
	if store == nil {
		return nil, errors.New("exonet: nil store")
	}
	if cfg.InstanceID == "" {
		return nil, fmt.Errorf("exonet: pool %q missing instance id", cfg.Pool.Name)
	}
	return &PoolAllocator{client: client, store: store, cfg: cfg}, nil
}

// Pool returns the pool this allocator manages.
func (a *PoolAllocator) Pool() external.ExternalPoolConfig { return a.cfg.Pool }

// Allocate creates a marked EIP, attaches it to this node, and records it.
//
// The EIP is created before the record is written. A crash in between leaves a
// marked EIP with no binding, which Reconcile finds and deletes; the reverse
// order could leave a binding naming an address that never existed.
func (a *PoolAllocator) Allocate(ctx context.Context, req external.AllocateRequest) (netip.Addr, error) {
	if req.PoolName != "" && req.PoolName != a.cfg.Pool.Name {
		return netip.Addr{}, fmt.Errorf("exonet: pool mismatch (got %q, bound to %q)", req.PoolName, a.cfg.Pool.Name)
	}

	eip, err := a.client.CreateElasticIP(ctx, a.marker(req))
	if err != nil {
		return netip.Addr{}, capacityErr(req.Purpose, fmt.Errorf("exonet: create elastic IP: %w", err), err)
	}

	if err := a.client.AttachElasticIP(ctx, a.cfg.InstanceID, eip.ID); err != nil {
		a.undo(ctx, eip, false)
		return netip.Addr{}, fmt.Errorf("exonet: attach elastic IP %s to %s: %w", eip.ID, a.cfg.InstanceID, err)
	}

	key := eip.Address.String()
	binding := Binding{
		EIPID:          eip.ID,
		Purpose:        req.Purpose,
		AllocationID:   req.AllocationID,
		ENIID:          req.ENIID,
		InstanceID:     req.InstanceID,
		NodeInstanceID: a.cfg.InstanceID,
	}
	err = a.store.Mutate(ctx, a.cfg.Pool.Name, func(rec *Record) (bool, error) {
		if rec.Bindings == nil {
			rec.Bindings = map[string]Binding{}
		}
		rec.Bindings[key] = binding
		return true, nil
	})
	if err != nil {
		a.undo(ctx, eip, true)
		return netip.Addr{}, fmt.Errorf("exonet: record binding for %s: %w", key, err)
	}

	slog.InfoContext(ctx, "exonet allocated external IP",
		"pool", a.cfg.Pool.Name, "public_ip", key, "eip_id", eip.ID, "purpose", req.Purpose)
	return eip.Address, nil
}

// Release detaches and deletes the EIP and drops the binding. ownerENIID scopes
// the release as StaticPoolAllocator.Release does, so a teardown sweep naming a
// stale owner is a no-op rather than a double-free.
//
// The record is dropped first: if the Exoscale calls then fail, the EIP is an
// unbound marked address that Reconcile collects, not a record that lies.
func (a *PoolAllocator) Release(ctx context.Context, poolName string, ip netip.Addr, ownerENIID string) error {
	if poolName != "" && poolName != a.cfg.Pool.Name {
		return fmt.Errorf("exonet: pool mismatch (got %q, bound to %q)", poolName, a.cfg.Pool.Name)
	}
	if !ip.IsValid() {
		return errors.New("exonet: invalid ip")
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
			return false, fmt.Errorf("exonet: %s not allocated in pool %s", key, a.cfg.Pool.Name)
		}
		if ownerENIID != "" && b.ENIID != "" && b.ENIID != ownerENIID {
			slog.InfoContext(ctx, "exonet release skip — address reassigned to a different ENI (stale release)",
				"pool", a.cfg.Pool.Name, "public_ip", key,
				"stale_owner_eni", ownerENIID, "current_owner_eni", b.ENIID)
			return false, nil
		}
		// Only ReleaseAddress, which names no interface, may free an Elastic IP.
		// Instance teardown must leave it allocated, as AWS does.
		if ownerENIID != "" && (b.Purpose == purposeEIP || b.AllocationID != "") {
			slog.InfoContext(ctx, "exonet release skip — address belongs to an Elastic IP allocation",
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

	if err := a.detachAndDelete(ctx, released.NodeInstanceID, released.EIPID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "exonet released external IP",
		"pool", a.cfg.Pool.Name, "public_ip", key, "eip_id", released.EIPID)
	return nil
}

// detachAndDelete returns an EIP to Exoscale. Not-found on either step is
// success: the address is already in the state we wanted. A failed detach is
// not final, because the delete may still succeed on an EIP nobody holds.
func (a *PoolAllocator) detachAndDelete(ctx context.Context, instanceID, eipID string) error {
	var detachErr error
	if instanceID != "" {
		if err := a.client.DetachElasticIP(ctx, instanceID, eipID); err != nil && !errors.Is(err, exoscale.ErrNotFound) {
			detachErr = fmt.Errorf("exonet: detach elastic IP %s: %w", eipID, err)
		}
	}
	if err := a.client.DeleteElasticIP(ctx, eipID); err != nil && !errors.Is(err, exoscale.ErrNotFound) {
		return errors.Join(detachErr, fmt.Errorf("exonet: delete elastic IP %s: %w", eipID, err))
	}
	if detachErr != nil {
		slog.WarnContext(ctx, "exonet: detach failed but the delete succeeded", "eip_id", eipID, "error", detachErr)
	}
	return nil
}

// undo rolls back a partial Allocate. Failures are logged, not returned: the
// caller is already failing, and the EIP keeps its marker for Reconcile.
func (a *PoolAllocator) undo(ctx context.Context, eip exoscale.ElasticIP, attached bool) {
	instanceID := ""
	if attached {
		instanceID = a.cfg.InstanceID
	}
	if err := a.detachAndDelete(ctx, instanceID, eip.ID); err != nil {
		slog.WarnContext(ctx, "exonet: could not roll back elastic IP; reconcile will collect it",
			"eip_id", eip.ID, "public_ip", eip.Address.String(), "error", err)
	}
}

// markerFor is the description prefix for EIPs attached to instanceID. The
// instance is in it so two Spinifex nodes in one organisation never judge each
// other's addresses.
func markerFor(instanceID string) string { return MarkerPrefix + instanceID + ":" }

// marker is this allocation's description: our prefix plus the AWS identity,
// so the Exoscale portal shows what claimed the address.
func (a *PoolAllocator) marker(req external.AllocateRequest) string {
	id := "unclaimed"
	switch {
	case req.AllocationID != "":
		id = req.AllocationID
	case req.ENIID != "":
		id = req.ENIID
	case req.InstanceID != "":
		id = req.InstanceID
	}
	return markerFor(a.cfg.InstanceID) + id
}

// ours reports whether an EIP carries this node's marker.
func (a *PoolAllocator) ours(e exoscale.ElasticIP) bool {
	return strings.HasPrefix(e.Description, markerFor(a.cfg.InstanceID))
}

// capacityErr maps the organisation quota onto the AWS code for the call. An
// Elastic IP allocation that hits a limit is AddressLimitExceeded in AWS; an
// auto-assigned address has no such limit there, so it reports capacity.
func capacityErr(purpose string, wrapped, raw error) error {
	if !errors.Is(raw, exoscale.ErrQuotaExceeded) {
		return wrapped
	}
	code := awserrors.ErrorInsufficientAddressCapacity
	if purpose == purposeEIP {
		code = awserrors.ErrorAddressLimitExceeded
	}
	return fmt.Errorf("%w: %w", errors.New(code), wrapped)
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
