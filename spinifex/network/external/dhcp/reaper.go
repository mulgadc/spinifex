package dhcp

import (
	"context"
	"fmt"
	"log/slog"
)

// OwnerStatus is the verdict on the resource a lease was taken for.
type OwnerStatus int

const (
	// OwnerUnknown means the owning resource could not be determined. Leases in
	// this state are always kept: a KV timeout must never read as a deletion.
	OwnerUnknown OwnerStatus = iota
	// OwnerAlive means the resource still exists and needs its address.
	OwnerAlive
	// OwnerGone means the resource is gone, so the lease holds an address
	// nothing will ever use or release.
	OwnerGone
)

func (s OwnerStatus) String() string {
	switch s {
	case OwnerAlive:
		return "alive"
	case OwnerGone:
		return "gone"
	default:
		return "unknown"
	}
}

// LeaseOwner resolves a lease back to the resource that justifies it. The
// manager holds leases but knows nothing about EIPs, ENIs or VPCs, so both the
// lookup and the datapath teardown belong to the caller.
type LeaseOwner interface {
	// Status reports whether the lease's resource still exists. An error is
	// reported as OwnerUnknown by the caller, never as OwnerGone.
	Status(ctx context.Context, e Entry) (OwnerStatus, error)
	// Discard removes whatever the lease configured, before the address goes
	// back. Called only for OwnerGone, and must be idempotent.
	Discard(ctx context.Context, e Entry) error
}

// SetLeaseOwner installs the resolver ReapOrphans consults. Without one,
// ReapOrphans reports every lease as unknown and reaps nothing.
func (m *Manager) SetLeaseOwner(o LeaseOwner) {
	m.mu.Lock()
	m.leaseOwner = o
	m.mu.Unlock()
}

// leaseOwnerRef copies the resolver out from under the lock, so a Discard that
// calls back into the manager cannot deadlock against it.
func (m *Manager) leaseOwnerRef() LeaseOwner {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leaseOwner
}

// Leases returns every lease this manager holds. For callers that reconcile a
// datapath against the addresses actually leased.
func (m *Manager) Leases(ctx context.Context) ([]Entry, error) {
	entries, err := m.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list leases: %w", err)
	}
	return entries, nil
}

// ReapOrphans releases leases whose owning resource no longer exists. Every
// other leak in this subsystem ends with an address held by nobody, so this is
// the only path that returns one without an operator. Returns the number
// released.
func (m *Manager) ReapOrphans(ctx context.Context) (int, error) {
	owner := m.leaseOwnerRef()
	if owner == nil {
		return 0, nil
	}
	entries, err := m.store.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list leases to reap: %w", err)
	}

	reaped := 0
	for _, e := range entries {
		if e.Lease == nil {
			continue
		}
		status, err := owner.Status(ctx, e)
		if err != nil {
			slog.Warn("dhcp reaper: owner lookup failed; keeping lease",
				"client_id", e.Lease.ClientID, "purpose", e.Purpose, "err", err)
			continue
		}
		if status != OwnerGone {
			continue
		}

		// The datapath goes first: releasing the address while a router port
		// still answers ARP for it invites a duplicate-IP conflict the moment
		// the server hands it out again.
		if err := owner.Discard(ctx, e); err != nil {
			slog.Error("dhcp reaper: discarding orphaned datapath failed; keeping lease",
				"client_id", e.Lease.ClientID, "purpose", e.Purpose, "ip", e.Lease.IP, "err", err)
			continue
		}
		if err := m.handleRelease(ctx, e.Lease.ClientID); err != nil {
			slog.Error("dhcp reaper: release failed",
				"client_id", e.Lease.ClientID, "ip", e.Lease.IP, "err", err)
			continue
		}
		slog.Warn("dhcp reaper: released lease whose owner is gone",
			"client_id", e.Lease.ClientID, "purpose", e.Purpose, "vpc_id", e.VPCID, "ip", e.Lease.IP)
		reaped++
	}
	if reaped > 0 {
		slog.Info("dhcp reaper: swept orphaned leases", "released", reaped, "total", len(entries))
	}
	return reaped, nil
}
