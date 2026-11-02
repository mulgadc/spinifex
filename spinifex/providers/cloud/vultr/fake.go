package vultr

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sync"
)

// Fake is an in-memory stand-in for Client with the semantics measured against
// the live API: Vultr picks the address, an address has at most one holder,
// re-attaching to the holder succeeds, detaching an unattached address
// succeeds, and deleting an attached address detaches it unless
// RefuseDeleteAttached is set.
type Fake struct {
	mu        sync.Mutex
	region    string
	quota     int
	next      netip.Addr
	seq       int
	ips       map[string]ReservedIP
	instances map[string]struct{}

	// Fail, when set for an operation name ("create", "get", "list", "attach",
	// "detach", "delete"), makes that operation return the error once.
	Fail map[string]error
	// Calls records operation names in order.
	Calls []string
	// RefuseDeleteAttached makes deleting an attached address fail with 400
	// and detach it, as the live API has been measured to do intermittently.
	RefuseDeleteAttached bool
}

// NewFake returns a fake for region with the given quota; zero is unlimited.
// instances are the hosts an address may be attached to.
func NewFake(region string, quota int, instances ...string) *Fake {
	f := &Fake{
		region:    region,
		quota:     quota,
		next:      netip.MustParseAddr("45.76.17.1"),
		ips:       map[string]ReservedIP{},
		instances: map[string]struct{}{},
		Fail:      map[string]error{},
	}
	for _, id := range instances {
		f.instances[id] = struct{}{}
	}
	return f
}

// Seed adds an address the caller did not create, such as an operator's own.
func (f *Fake) Seed(r ReservedIP) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Region == "" {
		r.Region = f.region
	}
	f.ips[r.ID] = r
}

// IDs returns every address ID, sorted.
func (f *Fake) IDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.ips))
	for id := range f.ips {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// HolderOf reports which instance holds id; empty when unattached or missing.
func (f *Fake) HolderOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ips[id].InstanceID
}

// fail must be called with f.mu held.
func (f *Fake) fail(op string) error {
	f.Calls = append(f.Calls, op)
	if err, ok := f.Fail[op]; ok {
		delete(f.Fail, op)
		return err
	}
	return nil
}

func notFound(what, id string) error {
	return &APIError{Status: 404, Message: fmt.Sprintf("invalid %s %s", what, id), kind: ErrNotFound}
}

func (f *Fake) CreateReservedIP(_ context.Context, label string) (ReservedIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("create"); err != nil {
		return ReservedIP{}, err
	}
	if f.quota > 0 && len(f.ips) >= f.quota {
		return ReservedIP{}, &APIError{Status: 400, Message: "reserved IP limit reached", kind: ErrQuotaExceeded}
	}
	f.seq++
	r := ReservedIP{ID: fmt.Sprintf("fake-rip-%04d", f.seq), Region: f.region, Label: label, Address: f.next}
	f.next = f.next.Next()
	f.ips[r.ID] = r
	return r, nil
}

func (f *Fake) GetReservedIP(_ context.Context, id string) (ReservedIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("get"); err != nil {
		return ReservedIP{}, err
	}
	r, ok := f.ips[id]
	if !ok {
		return ReservedIP{}, notFound("reserved-ip", id)
	}
	return r, nil
}

func (f *Fake) ListReservedIPs(_ context.Context) ([]ReservedIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("list"); err != nil {
		return nil, err
	}
	out := make([]ReservedIP, 0, len(f.ips))
	for _, r := range f.ips {
		if r.Region == f.region {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b ReservedIP) int { return a.Address.Compare(b.Address) })
	return out, nil
}

func (f *Fake) AttachReservedIP(_ context.Context, id, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("attach"); err != nil {
		return err
	}
	r, ok := f.ips[id]
	if !ok {
		return notFound("reserved-ip", id)
	}
	if _, ok := f.instances[instanceID]; !ok {
		return notFound("instance-id", instanceID)
	}
	switch r.InstanceID {
	case instanceID:
		return nil
	case "":
		r.InstanceID = instanceID
		f.ips[id] = r
		return nil
	}
	return fmt.Errorf("%w: %s is attached to %s", ErrHolderBusy, id, r.InstanceID)
}

func (f *Fake) DetachReservedIP(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("detach"); err != nil {
		return err
	}
	r, ok := f.ips[id]
	if !ok {
		return notFound("reserved-ip", id)
	}
	r.InstanceID = ""
	f.ips[id] = r
	return nil
}

func (f *Fake) DeleteReservedIP(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("delete"); err != nil {
		return err
	}
	r, ok := f.ips[id]
	if !ok {
		return notFound("reserved-ip", id)
	}
	if f.RefuseDeleteAttached && r.InstanceID != "" {
		r.InstanceID = ""
		f.ips[id] = r
		return &APIError{Status: 400, Message: "Unable to detach subnet, please restart your server from the control panel"}
	}
	delete(f.ips, id)
	return nil
}
