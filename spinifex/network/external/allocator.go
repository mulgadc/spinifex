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

// Allocator hands out a single external IP per AWS identity from a named
// pool. Implementations: StaticPoolAllocator (range math + KV CAS) and
// DHCPPoolAllocator (RFC 2131 DORA via vpcd, Q4).
type Allocator interface {
	Allocate(ctx context.Context, req AllocateRequest) (netip.Addr, error)
	Release(ctx context.Context, poolName string, ip netip.Addr) error
}
