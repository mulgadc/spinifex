package dhcp

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// Fake is a configurable in-memory Client for unit tests. It mimics the
// DORA / RENEW / RELEASE lifecycle well enough to exercise the Manager
// without opening real sockets. Hooks let tests inject errors or custom
// responses at each step.
type Fake struct {
	mu sync.Mutex

	// Default template for synthesised leases. Tests can override via
	// SetDefaultLease.
	defaultLease LeaseTemplate

	// Optional hooks — if non-nil the fake calls the hook instead of
	// synthesising a response. Useful for fault injection.
	AcquireHook func(AcquireRequest) (*Lease, error)
	RenewHook   func(*Lease) (*Lease, error)
	ReleaseHook func(*Lease) error

	// Tracked state.
	leases       map[string]*Lease // keyed by ClientID
	acquireCount int
	renewCount   int
	releaseCount int
}

// LeaseTemplate configures the synthetic lease Fake returns from Acquire
// when no AcquireHook is set.
type LeaseTemplate struct {
	IP            net.IP
	SubnetMask    net.IPMask
	Routers       []net.IP
	DNS           []net.IP
	ServerID      net.IP
	LeaseDuration time.Duration
}

// DefaultLeaseTemplate returns a reasonable template for tests (RFC 5737
// TEST-NET-1, 1 h lease).
func DefaultLeaseTemplate() LeaseTemplate {
	return LeaseTemplate{
		IP:            net.IPv4(192, 0, 2, 100),
		SubnetMask:    net.CIDRMask(24, 32),
		Routers:       []net.IP{net.IPv4(192, 0, 2, 1)},
		DNS:           []net.IP{net.IPv4(192, 0, 2, 1)},
		ServerID:      net.IPv4(192, 0, 2, 1),
		LeaseDuration: time.Hour,
	}
}

// NewFake creates a Fake pre-loaded with DefaultLeaseTemplate.
func NewFake() *Fake {
	return &Fake{
		defaultLease: DefaultLeaseTemplate(),
		leases:       map[string]*Lease{},
	}
}

// SetDefaultLease overrides the template used for subsequent Acquire calls.
func (f *Fake) SetDefaultLease(tmpl LeaseTemplate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultLease = tmpl
}

// AcquireCount returns the number of Acquire calls observed.
func (f *Fake) AcquireCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.acquireCount }

// RenewCount returns the number of Renew calls observed.
func (f *Fake) RenewCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.renewCount }

// ReleaseCount returns the number of Release calls observed.
func (f *Fake) ReleaseCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.releaseCount }

// HeldLease returns the last lease Fake synthesised for the given ClientID.
func (f *Fake) HeldLease(clientID string) (*Lease, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.leases[clientID]
	return l, ok
}

func (f *Fake) Acquire(_ context.Context, req AcquireRequest) (*Lease, error) {
	f.mu.Lock()
	f.acquireCount++
	hook := f.AcquireHook
	tmpl := f.defaultLease
	f.mu.Unlock()

	if hook != nil {
		lease, err := hook(req)
		if err == nil && lease != nil {
			f.mu.Lock()
			f.leases[req.ClientID] = lease
			f.mu.Unlock()
		}
		return lease, err
	}

	lease := &Lease{
		Bridge:        req.Bridge,
		ClientID:      req.ClientID,
		Hostname:      req.Hostname,
		VendorClass:   req.VendorClass,
		HWAddr:        append(net.HardwareAddr(nil), req.HWAddr...),
		IP:            tmpl.IP,
		SubnetMask:    tmpl.SubnetMask,
		Routers:       tmpl.Routers,
		DNS:           tmpl.DNS,
		ServerID:      tmpl.ServerID,
		AcquiredAt:    time.Now(),
		LeaseDuration: tmpl.LeaseDuration,
		T1:            tmpl.LeaseDuration / 2,
		T2:            tmpl.LeaseDuration * 7 / 8,
		// Synthetic raw packets — opaque marker bytes so Renew/Release
		// distinguish a fake lease from a nil-bytes one.
		RawOffer: []byte("fake-offer-" + req.ClientID),
		RawACK:   []byte("fake-ack-" + req.ClientID),
	}

	f.mu.Lock()
	f.leases[req.ClientID] = lease
	f.mu.Unlock()
	return lease, nil
}

func (f *Fake) Renew(_ context.Context, lease *Lease) (*Lease, error) {
	if lease == nil {
		return nil, fmt.Errorf("fake renew: lease is nil")
	}
	f.mu.Lock()
	f.renewCount++
	hook := f.RenewHook
	f.mu.Unlock()

	if hook != nil {
		renewed, err := hook(lease)
		if err == nil && renewed != nil {
			f.mu.Lock()
			f.leases[lease.ClientID] = renewed
			f.mu.Unlock()
		}
		return renewed, err
	}

	renewed := *lease
	renewed.AcquiredAt = time.Now()
	f.mu.Lock()
	f.leases[lease.ClientID] = &renewed
	f.mu.Unlock()
	return &renewed, nil
}

func (f *Fake) Release(_ context.Context, lease *Lease) error {
	if lease == nil {
		return nil
	}
	f.mu.Lock()
	f.releaseCount++
	hook := f.ReleaseHook
	f.mu.Unlock()
	if hook != nil {
		return hook(lease)
	}
	f.mu.Lock()
	delete(f.leases, lease.ClientID)
	f.mu.Unlock()
	return nil
}
