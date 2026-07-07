// Package dhcp provides an in-process DHCPv4 client (spinifex-vpcd) for
// single-attempt DORA/RENEW/RELEASE with KV-persisted leases. Retransmission
// is owned by DHCPManager one layer up.
package dhcp

import (
	"context"
	"net"
	"time"
)

// Client is the per-handshake DHCP surface. Backed by NewNClient4 in
// production; Fake satisfies this for unit tests.
type Client interface {
	Acquire(ctx context.Context, req AcquireRequest) (*Lease, error)
	Renew(ctx context.Context, lease *Lease) (*Lease, error)
	Release(ctx context.Context, lease *Lease) error
}

// AcquireRequest identifies a new DHCP lease. Fields map 1:1 to DHCP
// options and the payload chaddr.
type AcquireRequest struct {
	Bridge      string           // bridge carrying the AF_PACKET socket
	ClientID    string           // option 61 — client-identifier
	Hostname    string           // option 12 — host-name
	VendorClass string           // option 60 — vendor-class-identifier
	HWAddr      net.HardwareAddr // chaddr in the DHCP payload
	// UseIfaceMAC leases with the bind interface's own MAC instead of a
	// derived one — for uplinks that drop foreign source MACs (WiFi/WWAN).
	// Option 61 stays the discriminator between leases.
	UseIfaceMAC bool
}

// Lease is a server-bound DHCPv4 lease. Carries enough state for Renew
// and Release, plus the raw Offer/ACK packets so that DHCPManager (Q3)
// can resume after a daemon restart.
type Lease struct {
	Bridge      string
	ClientID    string
	Hostname    string
	VendorClass string
	HWAddr      net.HardwareAddr
	UseIfaceMAC bool

	IP         net.IP
	SubnetMask net.IPMask
	Routers    []net.IP
	DNS        []net.IP
	ServerID   net.IP

	AcquiredAt    time.Time
	LeaseDuration time.Duration
	T1            time.Duration
	T2            time.Duration

	// Raw wire packets. Opaque to callers; nclient4 needs them to build
	// RENEW / RELEASE messages that hit the same server binding. Persisted
	// in KV for crash recovery.
	RawOffer []byte
	RawACK   []byte
}

// ExpiresAt returns the absolute deadline at which the upstream server is
// free to reassign this IP.
func (l *Lease) ExpiresAt() time.Time { return l.AcquiredAt.Add(l.LeaseDuration) }

// RenewAt returns the absolute time the Manager should attempt a RENEW
// (option 58 / T1).
func (l *Lease) RenewAt() time.Time { return l.AcquiredAt.Add(l.T1) }

// RebindAt returns the absolute time the Manager should attempt a REBIND
// (option 59 / T2).
func (l *Lease) RebindAt() time.Time { return l.AcquiredAt.Add(l.T2) }
