package external

import (
	"context"
	"net/netip"
)

// AllocateRequest carries the allocation identity for static and DHCP allocators.
// PoolName selects the pool; AWS identity is recorded on the IPAM entry (static)
// or used as the DHCP client-ID.
type AllocateRequest struct {
	PoolName     string
	Purpose      string
	AllocationID string
	ENIID        string
	InstanceID   string
}

// OwnerTracker is an Allocator that also needs to hear about associations made
// after the address was allocated. An EIP is allocated with no ENI and attached
// later, so an allocator whose behaviour depends on the owner cannot learn it
// from Allocate alone.
//
// Only OCI needs this: the address is delivered to one VNIC, so the owning ENI
// is what decides which node that must be. A static or DHCP address is on the
// wire everywhere its node can reach, and those allocators do not implement it.
type OwnerTracker interface {
	// BindOwner records eniID as the current owner of ip in poolName. An empty
	// eniID clears the owner, which is what a disassociate leaves behind.
	BindOwner(ctx context.Context, poolName string, ip netip.Addr, eniID string) error
}

// Allocator hands out a single external IP per AWS identity from a named
// pool. Implementations: StaticPoolAllocator (range math + KV CAS) and
// DHCPPoolAllocator (RFC 2131 DORA via vpcd, Q4).
type Allocator interface {
	Allocate(ctx context.Context, req AllocateRequest) (netip.Addr, error)
	// Release frees ip back to poolName. ownerENIID, when non-empty, scopes the
	// release to the ENI that currently owns the lease: external IPs are recycled
	// and teardown sweeps re-emit releases, so a release naming an owner that no
	// longer matches the live allocation is a no-op (prevents double-freeing an
	// IP already reassigned to another instance). Empty ownerENIID releases
	// unconditionally.
	Release(ctx context.Context, poolName string, ip netip.Addr, ownerENIID string) error
}
