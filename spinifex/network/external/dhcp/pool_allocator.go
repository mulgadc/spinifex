package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/network/external"
)

// DHCPPoolAllocator implements external.Allocator for a DHCP-sourced pool,
// routing acquire/release via NATS to the bridge-owning vpcd. Client-id is
// derived from AllocationID / ENIID / InstanceID; Manager is idempotent.
type DHCPPoolAllocator struct {
	client *NATSClient
	pool   external.ExternalPoolConfig

	mu      sync.Mutex
	hwAddrs map[string]net.HardwareAddr // cached per client-id; vpcd's nclient4 fills its own when zero
}

var _ external.Allocator = (*DHCPPoolAllocator)(nil)

// NewDHCPPoolAllocator constructs an allocator bound to a single pool.
// Pool must have Source == external.SourceDHCP and a non-empty
// BindBridge; otherwise Allocate returns an error.
func NewDHCPPoolAllocator(client *NATSClient, pool external.ExternalPoolConfig) *DHCPPoolAllocator {
	return &DHCPPoolAllocator{
		client:  client,
		pool:    pool,
		hwAddrs: map[string]net.HardwareAddr{},
	}
}

// Pool returns the pool this allocator manages.
func (a *DHCPPoolAllocator) Pool() external.ExternalPoolConfig { return a.pool }

// Allocate issues vpc.dhcp.acquire over NATS and returns the leased IP.
func (a *DHCPPoolAllocator) Allocate(ctx context.Context, req external.AllocateRequest) (netip.Addr, error) {
	if a == nil || a.client == nil {
		return netip.Addr{}, errors.New("dhcp pool allocator: nil client")
	}
	if !a.pool.IsDHCP() {
		return netip.Addr{}, fmt.Errorf("dhcp pool allocator: pool %q is not dhcp-sourced", a.pool.Name)
	}
	if a.pool.BindBridge == "" {
		return netip.Addr{}, fmt.Errorf("dhcp pool allocator: pool %q missing bind_bridge", a.pool.Name)
	}
	if req.PoolName != "" && req.PoolName != a.pool.Name {
		return netip.Addr{}, fmt.Errorf("dhcp pool allocator: pool mismatch (got %q, bound to %q)", req.PoolName, a.pool.Name)
	}

	clientID := poolClientID(req)
	if clientID == "" {
		return netip.Addr{}, errors.New("dhcp pool allocator: AllocationID, ENIID or InstanceID required for client-id")
	}

	lease, err := a.client.RequestAcquire(ctx, AcquireParams{
		Bridge:   a.pool.BindBridge,
		ClientID: clientID,
		HWAddr:   a.hwAddrFor(clientID),
		Purpose:  poolPurpose(req),
		PoolName: a.pool.Name,
	})
	if err != nil {
		return netip.Addr{}, fmt.Errorf("dhcp pool allocate: %w", err)
	}
	if lease == nil || lease.IP == nil {
		return netip.Addr{}, errors.New("dhcp pool allocator: empty lease")
	}
	if len(lease.HWAddr) > 0 {
		a.rememberHWAddr(clientID, lease.HWAddr)
	}
	addr, ok := netip.AddrFromSlice(lease.IP.To4())
	if !ok {
		return netip.Addr{}, fmt.Errorf("dhcp pool allocator: parse leased ip %q", lease.IP)
	}
	return addr.Unmap(), nil
}

// Release issues vpc.dhcp.release for ip. Errors if poolName doesn't match
// this allocator's pool to prevent cross-pool release.
func (a *DHCPPoolAllocator) Release(ctx context.Context, poolName string, ip netip.Addr) error {
	if a == nil || a.client == nil {
		return errors.New("dhcp pool allocator: nil client")
	}
	if poolName != "" && poolName != a.pool.Name {
		return fmt.Errorf("dhcp pool allocator: pool mismatch (got %q, bound to %q)", poolName, a.pool.Name)
	}
	if !ip.IsValid() {
		return errors.New("dhcp pool allocator: invalid ip")
	}
	return a.client.RequestReleaseByIP(ctx, a.pool.Name, ip.String())
}

func (a *DHCPPoolAllocator) hwAddrFor(clientID string) net.HardwareAddr {
	a.mu.Lock()
	defer a.mu.Unlock()
	if hw, ok := a.hwAddrs[clientID]; ok && len(hw) > 0 {
		return hw
	}
	return nil
}

func (a *DHCPPoolAllocator) rememberHWAddr(clientID string, hw net.HardwareAddr) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hwAddrs[clientID] = append(net.HardwareAddr(nil), hw...)
}

// poolClientID picks the first non-empty AWS identifier. Mirrors the
// precedence documented in the plan: AllocationID > ENIID > InstanceID.
func poolClientID(req external.AllocateRequest) string {
	switch {
	case req.AllocationID != "":
		return req.AllocationID
	case req.ENIID != "":
		return req.ENIID
	case req.InstanceID != "":
		return req.InstanceID
	default:
		return ""
	}
}

func poolPurpose(req external.AllocateRequest) string {
	if req.Purpose != "" {
		return req.Purpose
	}
	switch {
	case req.AllocationID != "":
		return PurposeEIP
	case req.ENIID != "":
		return PurposeENIPublic
	case req.InstanceID != "":
		return PurposeENIPublic
	default:
		return ""
	}
}
