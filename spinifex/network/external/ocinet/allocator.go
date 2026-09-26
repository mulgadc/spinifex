// Package ocinet implements external.Allocator against the Oracle Cloud API.
//
// On bare metal an external pool is range math: the pool owns 72.52.77.233-254
// and handing one out is arithmetic plus a CAS. On OCI it is not. The
// hypervisor drops any packet whose source is not a registered private IP
// object on the sending VNIC, so an address only exists once OCI has been asked
// to create it. That makes allocation a round trip to a cloud API rather than a
// local computation, which is the whole reason this package exists alongside
// StaticPoolAllocator rather than replacing it.
//
// One allocation is two OCI objects. A secondary private IP on the VNIC is what
// rides the wire; a RESERVED public IP attached to it is what the internet
// reaches and what AWS calls the Elastic IP. Allocate returns the public
// address, because that is the address AWS semantics are about — the private
// half is carried in the binding record, and Lookup is how vpcd finds it when
// it programs the OVN NAT rule and the host ingress route.
//
// The host does not configure the private address on an interface. Owning it
// would make the host terminate the guest's traffic — an SSH to the public
// address would reach the node, not the instance. It is routed instead: a /32
// via the VPC gateway LRP over the transit veth, which is what EnsureEIPIngress
// already installs for a routed-mode EIP.
package ocinet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/netip"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/mulgadc/spinifex/spinifex/network/external"
)

// DisplayNamePrefix marks every OCI object this package creates. Reconcile only
// ever deletes objects carrying it, so an operator's own secondary addresses on
// the same VNIC — mulga-poc has two — are never candidates for cleanup. A
// prefix rather than a freeform tag because it is visible in the OCI console,
// where whoever is wondering what created an address will actually look.
const DisplayNamePrefix = "spinifex-"

// purposeEIP duplicates handlers/ec2/vpc.PurposeEIP so the allocator can tell
// an Elastic IP from an auto-assigned address without importing handlers.
const purposeEIP = "eip"

// assignSchedule is the poll ladder for an address to reach ASSIGNED. OCI
// assignment is asynchronous: CreatePublicIp returns ASSIGNING and the address
// is not reachable until it settles. Shaped like the DHCP manager's DORA ladder
// and bounded by the same reasoning — allocation sits inside the AWS client's
// read timeout, so the budget has to leave room for the reply.
var assignSchedule = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// assignBudget caps the wallclock for one Allocate. Sized against the same 60s
// botocore read timeout that gives dhcp.defaultAcquireBudget its 45s.
const assignBudget = 45 * time.Second

// assignJitter is the ± window on each poll so concurrent allocations across
// nodes do not synchronise onto the same API second.
const assignJitter = 200 * time.Millisecond

// Binding is one allocated external address: the pair of OCI objects behind it
// plus the AWS identity that claimed it. Keyed in the record by the public
// address, mirroring StaticPoolAllocator.Allocated, so Release is a direct
// lookup and the ownership guard reads the same way.
type Binding struct {
	PublicIPID  string `json:"public_ip_id"`
	PrivateIPID string `json:"private_ip_id"`
	// PrivateAddr is what the host configures on the VNIC and what inbound
	// packets actually carry — OCI NATs the public address to it before the
	// instance ever sees the packet, so the DNAT rule matches this, not the
	// public address.
	PrivateAddr  string `json:"private_addr"`
	Purpose      string `json:"purpose,omitempty"`
	AllocationID string `json:"allocation_id,omitempty"`
	ENIID        string `json:"eni_id,omitempty"`
	InstanceID   string `json:"instance_id,omitempty"`
	// VNICID is the VNIC currently carrying the private IP, and so the node
	// the address lives on. A private IP can be reassigned to another VNIC in
	// the same subnet without losing its public IP, which is how an address
	// follows an instance to a surviving node — and this field is how a node
	// that comes back from the dead tells "mine" from "moved on without me"
	// before it reasserts host plumbing for an address it no longer holds.
	VNICID string `json:"vnic_id,omitempty"`
}

// Config wires one pool to one VNIC in one compartment.
type Config struct {
	// Pool is the external pool this allocator serves. Source must be "oci".
	Pool external.ExternalPoolConfig
	// VNICID is the OCID of the VNIC that carries external traffic. On the
	// reference topology that is the second VNIC, the one in br-wan.
	VNICID string
	// CompartmentID is where reserved public IPs are created.
	CompartmentID string

	// LocalPorts returns the OVN logical ports with a live tap on this host.
	// It is how ClaimLocalAddresses knows which addresses belong here, and the
	// same authority the host EIP binder uses for the same question. Nil
	// disables the affinity pass, which is correct for a single-node cluster
	// and for tests that only exercise allocate and release.
	LocalPorts func(ctx context.Context) (map[string]struct{}, error)

	// Schedule and Budget override the poll ladder; zero values take the
	// package defaults. Tests set them to keep runtime down.
	Schedule []time.Duration
	Budget   time.Duration
	// Sleep is the delay function, so tests do not wait.
	Sleep func(context.Context, time.Duration) error
}

// Store persists bindings. Implemented over JetStream KV in store.go; the
// interface exists so the allocator's tests need no NATS.
type Store interface {
	// Mutate applies fn to the pool's binding set under CAS, retrying on
	// contention. fn reports whether it changed anything.
	Mutate(ctx context.Context, poolName string, fn func(*Record) (bool, error)) error
	// Get reads the current record.
	Get(ctx context.Context, poolName string) (Record, error)
}

// Record is the per-pool binding set.
type Record struct {
	Bindings map[string]Binding `json:"bindings"`
}

// PoolAllocator implements external.Allocator against OCI.
type PoolAllocator struct {
	client oci.Client
	store  Store
	cfg    Config

	schedule   []time.Duration
	budget     time.Duration
	sleep      func(context.Context, time.Duration) error
	localPorts func(context.Context) (map[string]struct{}, error)
}

var (
	_ external.Allocator    = (*PoolAllocator)(nil)
	_ external.OwnerTracker = (*PoolAllocator)(nil)
)

// New constructs an allocator bound to a single pool.
func New(client oci.Client, store Store, cfg Config) (*PoolAllocator, error) {
	if client == nil {
		return nil, errors.New("ocinet: nil OCI client")
	}
	if store == nil {
		return nil, errors.New("ocinet: nil store")
	}
	if cfg.VNICID == "" {
		return nil, fmt.Errorf("ocinet: pool %q missing vnic_id", cfg.Pool.Name)
	}
	if cfg.CompartmentID == "" {
		return nil, fmt.Errorf("ocinet: pool %q missing compartment_id", cfg.Pool.Name)
	}
	a := &PoolAllocator{
		client:     client,
		store:      store,
		cfg:        cfg,
		schedule:   cfg.Schedule,
		budget:     cfg.Budget,
		sleep:      cfg.Sleep,
		localPorts: cfg.LocalPorts,
	}
	if len(a.schedule) == 0 {
		a.schedule = assignSchedule
	}
	if a.budget == 0 {
		a.budget = assignBudget
	}
	if a.sleep == nil {
		a.sleep = sleepCtx
	}
	return a, nil
}

// Pool returns the pool this allocator manages.
func (a *PoolAllocator) Pool() external.ExternalPoolConfig { return a.cfg.Pool }

// Allocate creates a secondary private IP on the VNIC, attaches a reserved
// public IP to it, waits for the assignment to settle, and records the pair.
//
// The OCI objects are created before the record is written, deliberately. The
// inverse order would let a crash leave a record naming addresses that do not
// exist, which nothing can detect — whereas a crash here leaves real objects
// carrying our display-name prefix and no record, which Reconcile finds by
// listing the VNIC. A leak that is discoverable beats a record that lies.
func (a *PoolAllocator) Allocate(ctx context.Context, req external.AllocateRequest) (netip.Addr, error) {
	if req.PoolName != "" && req.PoolName != a.cfg.Pool.Name {
		return netip.Addr{}, fmt.Errorf("ocinet: pool mismatch (got %q, bound to %q)", req.PoolName, a.cfg.Pool.Name)
	}

	name := displayName(req)
	priv, err := a.client.AssignPrivateIP(ctx, a.cfg.VNICID, netip.Addr{}, name)
	if err != nil {
		return netip.Addr{}, capacityErr(fmt.Errorf("ocinet: assign private ip: %w", err), err)
	}

	pub, err := a.client.CreatePublicIP(ctx, a.cfg.CompartmentID, priv.ID, name)
	if err != nil {
		// Give the private IP back rather than leaving it for Reconcile: we are
		// still here and we know exactly what we just made.
		a.undoPrivate(ctx, priv.ID)
		return netip.Addr{}, capacityErr(fmt.Errorf("ocinet: create public ip: %w", err), err)
	}

	pub, err = a.waitAssigned(ctx, pub)
	if err != nil {
		a.undoPublic(ctx, pub.ID)
		a.undoPrivate(ctx, priv.ID)
		return netip.Addr{}, err
	}
	if !pub.Address.IsValid() {
		a.undoPublic(ctx, pub.ID)
		a.undoPrivate(ctx, priv.ID)
		return netip.Addr{}, fmt.Errorf("ocinet: public ip %s has no address", pub.ID)
	}

	key := pub.Address.String()
	binding := Binding{
		PublicIPID:   pub.ID,
		PrivateIPID:  priv.ID,
		PrivateAddr:  priv.Address.String(),
		Purpose:      req.Purpose,
		AllocationID: req.AllocationID,
		ENIID:        req.ENIID,
		InstanceID:   req.InstanceID,
		VNICID:       priv.VNICID,
	}
	err = a.store.Mutate(ctx, a.cfg.Pool.Name, func(rec *Record) (bool, error) {
		if rec.Bindings == nil {
			rec.Bindings = map[string]Binding{}
		}
		rec.Bindings[key] = binding
		return true, nil
	})
	if err != nil {
		a.undoPublic(ctx, pub.ID)
		a.undoPrivate(ctx, priv.ID)
		return netip.Addr{}, fmt.Errorf("ocinet: record binding for %s: %w", key, err)
	}

	slog.InfoContext(ctx, "ocinet allocated external IP",
		"pool", a.cfg.Pool.Name, "public_ip", key, "private_ip", binding.PrivateAddr,
		"purpose", req.Purpose, "public_ip_id", pub.ID, "private_ip_id", priv.ID)
	return pub.Address, nil
}

// Release deletes both OCI objects and drops the binding. ownerENIID scopes the
// release exactly as StaticPoolAllocator.Release does: external addresses are
// recycled and teardown sweeps re-emit releases, so a release naming an owner
// that no longer holds the binding is a no-op rather than a double-free of an
// address already handed to another instance.
//
// The record is dropped first here, the opposite of Allocate, and for the same
// reason: whichever half survives a crash should be the discoverable one. A
// binding removed with the objects still present is a leak Reconcile collects;
// objects removed with the binding still present would be a record that lies.
func (a *PoolAllocator) Release(ctx context.Context, poolName string, ip netip.Addr, ownerENIID string) error {
	if poolName != "" && poolName != a.cfg.Pool.Name {
		return fmt.Errorf("ocinet: pool mismatch (got %q, bound to %q)", poolName, a.cfg.Pool.Name)
	}
	if !ip.IsValid() {
		return errors.New("ocinet: invalid ip")
	}
	key := ip.String()

	var released Binding
	var found bool
	err := a.store.Mutate(ctx, a.cfg.Pool.Name, func(rec *Record) (bool, error) {
		// Reset per attempt: a CAS retry that finds the binding already gone
		// must not inherit the previous attempt's verdict.
		released, found = Binding{}, false

		b, ok := rec.Bindings[key]
		if !ok {
			if ownerENIID != "" {
				return false, nil
			}
			return false, fmt.Errorf("ocinet: %s not allocated in pool %s", key, a.cfg.Pool.Name)
		}
		if ownerENIID != "" && b.ENIID != "" && b.ENIID != ownerENIID {
			slog.InfoContext(ctx, "ocinet release skip — address reassigned to a different ENI (stale release)",
				"pool", a.cfg.Pool.Name, "public_ip", key,
				"stale_owner_eni", ownerENIID, "current_owner_eni", b.ENIID)
			return false, nil
		}
		// Only an unscoped release — ReleaseAddress, which names no interface —
		// may free an EIP. Here the address is a reserved public IP object, so
		// freeing one on instance teardown destroys it at the provider and the
		// customer cannot get it back.
		// AllocationID is set only for an EIP, so it stands in when a caller
		// allocated without naming a purpose.
		if ownerENIID != "" && (b.Purpose == purposeEIP || b.AllocationID != "") {
			slog.InfoContext(ctx, "ocinet release skip — address belongs to an Elastic IP allocation",
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

	if err := a.deletePair(ctx, released.PublicIPID, released.PrivateIPID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "ocinet released external IP",
		"pool", a.cfg.Pool.Name, "public_ip", key, "private_ip", released.PrivateAddr)
	return nil
}

// BindOwner records which ENI an already-allocated address is attached to.
//
// Allocate learns the owner only when one exists at the time — true for an
// auto-assigned address, never for an EIP, which is allocated bare and attached
// by a later AssociateAddress. Without this the binding of every EIP names no
// ENI, so ClaimLocalAddresses cannot tell which node should hold it and the
// address stays wherever the allocating node happened to put it.
//
// An empty eniID clears the owner, which is the state a disassociate leaves: the
// address is still allocated and still billing, but no guest answers on it and
// no node should claim it.
func (a *PoolAllocator) BindOwner(ctx context.Context, poolName string, ip netip.Addr, eniID string) error {
	if poolName != "" && poolName != a.cfg.Pool.Name {
		return fmt.Errorf("ocinet: pool mismatch (got %q, bound to %q)", poolName, a.cfg.Pool.Name)
	}
	if !ip.IsValid() {
		return errors.New("ocinet: invalid ip")
	}
	key := ip.String()
	return a.store.Mutate(ctx, a.cfg.Pool.Name, func(rec *Record) (bool, error) {
		b, ok := rec.Bindings[key]
		// An address this pool never allocated is not ours to annotate, and an
		// unchanged owner is not worth a KV write on every re-association.
		if !ok || b.ENIID == eniID {
			return false, nil
		}
		b.ENIID = eniID
		rec.Bindings[key] = b
		return true, nil
	})
}

// BindingFor returns the OCI pair behind an allocated public address. The host
// plumbing needs the private half: inbound packets arrive carrying it, not the
// public address, because OCI has already NATed by the time the instance sees
// them.
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

// waitAssigned polls until the public IP reports ASSIGNED, or the budget runs
// out. A create that has already settled returns without a single extra call.
func (a *PoolAllocator) waitAssigned(ctx context.Context, pub oci.PublicIP) (oci.PublicIP, error) {
	if pub.IsAssigned() {
		return pub, nil
	}
	deadline := time.Now().Add(a.budget)
	for i := 0; ; i++ {
		wait := a.schedule[min(i, len(a.schedule)-1)]
		if j := jitter(assignJitter); wait+j > 0 {
			wait += j
		}
		if time.Now().Add(wait).After(deadline) {
			return pub, fmt.Errorf("ocinet: public ip %s still %s after %s",
				pub.ID, pub.LifecycleState, a.budget)
		}
		if err := a.sleep(ctx, wait); err != nil {
			return pub, err
		}
		got, err := a.client.GetPublicIP(ctx, pub.ID)
		if err != nil {
			return pub, fmt.Errorf("ocinet: poll public ip %s: %w", pub.ID, err)
		}
		if got.IsAssigned() {
			return got, nil
		}
		pub = got
	}
}

// deletePair removes the public IP then the private IP. Public first: deleting
// a private IP that still has a public attached is refused by OCI, and the
// reverse order would strand the reserved address with nothing pointing at it.
// A 404 on either is success — the address is already in the state we wanted.
func (a *PoolAllocator) deletePair(ctx context.Context, publicIPID, privateIPID string) error {
	if publicIPID != "" {
		if err := a.client.DeletePublicIP(ctx, publicIPID); err != nil && !errors.Is(err, oci.ErrNotFound) {
			return fmt.Errorf("ocinet: delete public ip %s: %w", publicIPID, err)
		}
	}
	if privateIPID != "" {
		if err := a.client.UnassignPrivateIP(ctx, privateIPID); err != nil && !errors.Is(err, oci.ErrNotFound) {
			return fmt.Errorf("ocinet: unassign private ip %s: %w", privateIPID, err)
		}
	}
	return nil
}

// undoPrivate and undoPublic roll back a partial Allocate. Failures are logged
// and swallowed: the caller is already returning an error, and the object is
// left carrying our prefix so Reconcile collects it on the next pass. Turning a
// cleanup failure into the reported error would hide why the allocation failed.
func (a *PoolAllocator) undoPrivate(ctx context.Context, privateIPID string) {
	if err := a.client.UnassignPrivateIP(ctx, privateIPID); err != nil && !errors.Is(err, oci.ErrNotFound) {
		slog.WarnContext(ctx, "ocinet: could not roll back private ip; reconcile will collect it",
			"private_ip_id", privateIPID, "error", err)
	}
}

func (a *PoolAllocator) undoPublic(ctx context.Context, publicIPID string) {
	if publicIPID == "" {
		return
	}
	if err := a.client.DeletePublicIP(ctx, publicIPID); err != nil && !errors.Is(err, oci.ErrNotFound) {
		slog.WarnContext(ctx, "ocinet: could not roll back public ip; reconcile will collect it",
			"public_ip_id", publicIPID, "error", err)
	}
}

// capacityErr maps an OCI quota refusal onto the AWS error that means the same
// thing, so a customer who has hit the 64-per-VNIC or 50-per-region ceiling is
// told they are out of addresses rather than shown an Oracle error code.
func capacityErr(wrapped, raw error) error {
	if errors.Is(raw, oci.ErrLimitExceeded) {
		return fmt.Errorf("%w: %w", errors.New(awserrors.ErrorInsufficientAddressCapacity), wrapped)
	}
	return wrapped
}

// displayName marks the object as ours and carries the AWS identity, so the OCI
// console shows what claimed the address.
func displayName(req external.AllocateRequest) string {
	switch {
	case req.AllocationID != "":
		return DisplayNamePrefix + req.AllocationID
	case req.ENIID != "":
		return DisplayNamePrefix + req.ENIID
	case req.InstanceID != "":
		return DisplayNamePrefix + req.InstanceID
	default:
		return DisplayNamePrefix + "unclaimed"
	}
}

func jitter(span time.Duration) time.Duration {
	if span <= 0 {
		return 0
	}
	// Spreading the poll ladder, not generating a secret: math/rand is the
	// right tool and crypto/rand would be cargo cult here.
	return time.Duration(rand.Int64N(int64(2*span))) - span //nolint:gosec // jitter, not a secret
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
