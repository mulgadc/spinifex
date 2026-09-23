package oci

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
)

// Fake is an in-memory Client for tests. It is in the production package
// rather than a _test file because the allocator package needs it too, and it
// models the two behaviours that actually bite: OCI picks the address, and
// assignment is asynchronous so a create is not immediately ASSIGNED.
type Fake struct {
	mu sync.Mutex

	// NextAddr supplies successive private addresses when the caller does not
	// name one, standing in for OCI's own choice from the subnet.
	NextAddr netip.Addr
	// NextPublic supplies successive public addresses.
	NextPublic netip.Addr

	// AssignAfter is how many Get calls a public IP spends in ASSIGNING before
	// reporting ASSIGNED. Zero means assigned immediately.
	AssignAfter int

	// FailWith, when non-nil, is returned by the next call to the named
	// operation and then cleared. Keyed by the Op strings used in APIError.
	FailWith map[string]error

	// PrivateLimit caps private IPs per VNIC; 0 means unlimited. OCI's real
	// cap is 64.
	PrivateLimit int

	privateIPs map[string]PrivateIP // by OCID
	publicIPs  map[string]PublicIP  // by OCID
	pending    map[string]int       // public OCID -> Gets remaining before ASSIGNED
	seq        int
}

var _ Client = (*Fake)(nil)

// NewFake returns a Fake seeded with sensible address counters.
func NewFake() *Fake {
	return &Fake{
		NextAddr:   netip.MustParseAddr("10.200.0.100"),
		NextPublic: netip.MustParseAddr("203.0.113.10"),
		FailWith:   map[string]error{},
		privateIPs: map[string]PrivateIP{},
		publicIPs:  map[string]PublicIP{},
		pending:    map[string]int{},
	}
}

// SeedPrivateIP inserts a private IP directly, for reconcile tests that need
// OCI to already hold state our KV does not know about.
func (f *Fake) SeedPrivateIP(p PrivateIP) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.privateIPs[p.ID] = p
}

// SeedPublicIP inserts a public IP directly.
func (f *Fake) SeedPublicIP(p PublicIP) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publicIPs[p.ID] = p
}

// PrivateIPs returns a snapshot, so a test can assert nothing leaked.
func (f *Fake) PrivateIPs() []PrivateIP {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PrivateIP, 0, len(f.privateIPs))
	for _, p := range f.privateIPs {
		out = append(out, p)
	}
	return out
}

// PublicIPs returns a snapshot.
func (f *Fake) PublicIPs() []PublicIP {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PublicIP, 0, len(f.publicIPs))
	for _, p := range f.publicIPs {
		out = append(out, p)
	}
	return out
}

// take returns and clears a staged failure for op.
func (f *Fake) take(op string) error {
	if err, ok := f.FailWith[op]; ok {
		delete(f.FailWith, op)
		return err
	}
	return nil
}

func (f *Fake) next() int { f.seq++; return f.seq }

func (f *Fake) AssignPrivateIP(_ context.Context, vnicID string, addr netip.Addr, displayName string) (PrivateIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("AssignPrivateIP"); err != nil {
		return PrivateIP{}, err
	}
	if f.PrivateLimit > 0 {
		n := 0
		for _, p := range f.privateIPs {
			if p.VNICID == vnicID {
				n++
			}
		}
		if n >= f.PrivateLimit {
			return PrivateIP{}, &APIError{
				Op: "AssignPrivateIP", Code: "LimitExceeded", StatusCode: 400,
				Message: fmt.Sprintf("vnic %s already has %d private IPs", vnicID, n),
				kind:    ErrLimitExceeded,
			}
		}
	}
	if !addr.IsValid() {
		addr = f.NextAddr
		f.NextAddr = f.NextAddr.Next()
	}
	for _, p := range f.privateIPs {
		if p.Address == addr {
			return PrivateIP{}, &APIError{
				Op: "AssignPrivateIP", Code: "Conflict", StatusCode: 409,
				Message: fmt.Sprintf("address %s already assigned", addr), kind: ErrConflict,
			}
		}
	}
	p := PrivateIP{
		ID:          fmt.Sprintf("ocid1.privateip.oc1..fake%03d", f.next()),
		Address:     addr,
		VNICID:      vnicID,
		DisplayName: displayName,
	}
	f.privateIPs[p.ID] = p
	return p, nil
}

func (f *Fake) UnassignPrivateIP(_ context.Context, privateIPID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("UnassignPrivateIP"); err != nil {
		return err
	}
	p, ok := f.privateIPs[privateIPID]
	if !ok {
		return &APIError{Op: "UnassignPrivateIP", StatusCode: 404, Message: "no such private ip", kind: ErrNotFound}
	}
	if p.IsPrimary {
		return &APIError{Op: "UnassignPrivateIP", StatusCode: 409, Message: "cannot delete the primary private ip", kind: ErrConflict}
	}
	delete(f.privateIPs, privateIPID)
	return nil
}

func (f *Fake) GetPrivateIP(_ context.Context, privateIPID string) (PrivateIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("GetPrivateIP"); err != nil {
		return PrivateIP{}, err
	}
	p, ok := f.privateIPs[privateIPID]
	if !ok {
		return PrivateIP{}, &APIError{Op: "GetPrivateIP", StatusCode: 404, Message: "no such private ip", kind: ErrNotFound}
	}
	return p, nil
}

func (f *Fake) ListPrivateIPs(_ context.Context, vnicID string) ([]PrivateIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("ListPrivateIPs"); err != nil {
		return nil, err
	}
	var out []PrivateIP
	for _, p := range f.privateIPs {
		if p.VNICID == vnicID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *Fake) CreatePublicIP(_ context.Context, compartmentID, privateIPID, displayName string) (PublicIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("CreatePublicIP"); err != nil {
		return PublicIP{}, err
	}
	_ = compartmentID
	addr := f.NextPublic
	f.NextPublic = f.NextPublic.Next()
	p := PublicIP{
		ID:             fmt.Sprintf("ocid1.publicip.oc1..fake%03d", f.next()),
		Address:        addr,
		PrivateIPID:    privateIPID,
		DisplayName:    displayName,
		Lifetime:       LifetimeReserved,
		LifecycleState: LifecycleStateAssigned,
	}
	if privateIPID == "" {
		p.LifecycleState = LifecycleStateAvailable
	} else if f.AssignAfter > 0 {
		p.LifecycleState = LifecycleStateAssigning
		f.pending[p.ID] = f.AssignAfter
	}
	f.publicIPs[p.ID] = p
	return p, nil
}

func (f *Fake) AttachPublicIP(_ context.Context, publicIPID, privateIPID string) (PublicIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("AttachPublicIP"); err != nil {
		return PublicIP{}, err
	}
	p, ok := f.publicIPs[publicIPID]
	if !ok {
		return PublicIP{}, &APIError{Op: "AttachPublicIP", StatusCode: 404, Message: "no such public ip", kind: ErrNotFound}
	}
	p.PrivateIPID = privateIPID
	p.LifecycleState = LifecycleStateAssigned
	if f.AssignAfter > 0 {
		p.LifecycleState = LifecycleStateAssigning
		f.pending[p.ID] = f.AssignAfter
	}
	f.publicIPs[publicIPID] = p
	return p, nil
}

func (f *Fake) DetachPublicIP(_ context.Context, publicIPID string) (PublicIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("DetachPublicIP"); err != nil {
		return PublicIP{}, err
	}
	p, ok := f.publicIPs[publicIPID]
	if !ok {
		return PublicIP{}, &APIError{Op: "DetachPublicIP", StatusCode: 404, Message: "no such public ip", kind: ErrNotFound}
	}
	p.PrivateIPID = ""
	p.LifecycleState = LifecycleStateAvailable
	f.publicIPs[publicIPID] = p
	return p, nil
}

func (f *Fake) DeletePublicIP(_ context.Context, publicIPID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("DeletePublicIP"); err != nil {
		return err
	}
	if _, ok := f.publicIPs[publicIPID]; !ok {
		return &APIError{Op: "DeletePublicIP", StatusCode: 404, Message: "no such public ip", kind: ErrNotFound}
	}
	delete(f.publicIPs, publicIPID)
	delete(f.pending, publicIPID)
	return nil
}

func (f *Fake) GetPublicIP(_ context.Context, publicIPID string) (PublicIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("GetPublicIP"); err != nil {
		return PublicIP{}, err
	}
	p, ok := f.publicIPs[publicIPID]
	if !ok {
		return PublicIP{}, &APIError{Op: "GetPublicIP", StatusCode: 404, Message: "no such public ip", kind: ErrNotFound}
	}
	if n, pendingNow := f.pending[publicIPID]; pendingNow {
		if n <= 1 {
			delete(f.pending, publicIPID)
			p.LifecycleState = LifecycleStateAssigned
			f.publicIPs[publicIPID] = p
		} else {
			f.pending[publicIPID] = n - 1
		}
	}
	return p, nil
}

func (f *Fake) GetPublicIPByPrivateIPID(_ context.Context, privateIPID string) (PublicIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("GetPublicIPByPrivateIPID"); err != nil {
		return PublicIP{}, err
	}
	for _, p := range f.publicIPs {
		if p.PrivateIPID == privateIPID {
			return p, nil
		}
	}
	return PublicIP{}, &APIError{Op: "GetPublicIPByPrivateIPID", StatusCode: 404, Message: "no public ip for private ip", kind: ErrNotFound}
}
