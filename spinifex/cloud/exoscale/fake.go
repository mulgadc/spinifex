package exoscale

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sync"
)

// Fake is an in-memory Client for tests. It enforces the organisation quota
// and the attach relationship, which are the two behaviours the allocator's
// error and rollback paths depend on.
type Fake struct {
	mu       sync.Mutex
	zone     string
	quota    int
	next     netip.Addr
	seq      int
	eips     map[string]ElasticIP
	attached map[string]string // eip ID -> instance ID

	// Fail, when set for an operation name ("create", "attach", "detach",
	// "delete", "list"), makes that operation return the error once.
	Fail map[string]error
	// Calls records operation names in order.
	Calls []string
}

var _ Client = (*Fake)(nil)

// NewFake returns a fake with the given quota; zero means unlimited.
func NewFake(zone string, quota int) *Fake {
	return &Fake{
		zone:     zone,
		quota:    quota,
		next:     netip.MustParseAddr("194.182.160.1"),
		eips:     map[string]ElasticIP{},
		attached: map[string]string{},
		Fail:     map[string]error{},
	}
}

// Seed adds an EIP the allocator did not create, such as an operator's own.
func (f *Fake) Seed(e ElasticIP) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eips[e.ID] = e
}

// AttachedTo reports which instance an EIP is attached to, if any.
func (f *Fake) AttachedTo(eipID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inst, ok := f.attached[eipID]
	return inst, ok
}

// IDs returns every EIP ID, sorted.
func (f *Fake) IDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.eips))
	for id := range f.eips {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (f *Fake) fail(op string) error {
	f.Calls = append(f.Calls, op)
	if err, ok := f.Fail[op]; ok {
		delete(f.Fail, op)
		return err
	}
	return nil
}

func (f *Fake) CreateElasticIP(_ context.Context, description string) (ElasticIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("create"); err != nil {
		return ElasticIP{}, err
	}
	if f.quota > 0 && len(f.eips) >= f.quota {
		return ElasticIP{}, fmt.Errorf("%w: Conflict: Usage of resource 'eip' has been exceeded", ErrQuotaExceeded)
	}
	f.seq++
	e := ElasticIP{
		ID:          fmt.Sprintf("00000000-0000-4000-8000-%012d", f.seq),
		Address:     f.next,
		Description: description,
		Zone:        f.zone,
	}
	f.next = f.next.Next()
	f.eips[e.ID] = e
	return e, nil
}

func (f *Fake) AttachElasticIP(_ context.Context, instanceID, eipID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("attach"); err != nil {
		return err
	}
	if _, ok := f.eips[eipID]; !ok {
		return fmt.Errorf("%w: elastic IP %s", ErrNotFound, eipID)
	}
	f.attached[eipID] = instanceID
	return nil
}

func (f *Fake) DetachElasticIP(_ context.Context, instanceID, eipID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("detach"); err != nil {
		return err
	}
	if f.attached[eipID] != instanceID {
		return fmt.Errorf("%w: elastic IP %s is not attached to %s", ErrNotFound, eipID, instanceID)
	}
	delete(f.attached, eipID)
	return nil
}

func (f *Fake) DeleteElasticIP(_ context.Context, eipID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("delete"); err != nil {
		return err
	}
	if _, ok := f.eips[eipID]; !ok {
		return fmt.Errorf("%w: elastic IP %s", ErrNotFound, eipID)
	}
	delete(f.eips, eipID)
	delete(f.attached, eipID)
	return nil
}

func (f *Fake) ListElasticIPs(context.Context) ([]ElasticIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("list"); err != nil {
		return nil, err
	}
	out := make([]ElasticIP, 0, len(f.eips))
	for _, e := range f.eips {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b ElasticIP) int { return a.Address.Compare(b.Address) })
	return out, nil
}

func (f *Fake) Version(context.Context) (string, error) { return "exo fake", nil }
