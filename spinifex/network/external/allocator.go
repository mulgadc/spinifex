package external

import (
	"context"
	"net/netip"
)

// AllocateRequest carries the per-allocation identity used by both static
// and DHCP-backed allocators. PoolName picks the pool; the AWS identity
// (AllocationID / ENIID / InstanceID) is recorded on the IPAM entry for
// static pools and used as the DHCP client-ID for DHCP pools.
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
