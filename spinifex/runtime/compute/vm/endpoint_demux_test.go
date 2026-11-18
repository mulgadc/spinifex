package vm

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeDemuxer models the bridge: the flows each ENI's tap holds.
type fakeDemuxer struct {
	mu      sync.Mutex
	flows   map[string][]netip.AddrPort
	unowned int
	readErr error
	sets    []string
}

var _ EndpointDemuxer = (*fakeDemuxer)(nil)

func (f *fakeDemuxer) EndpointDemuxStates(_ context.Context, eniIDs []string) (EndpointDemuxStates, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return EndpointDemuxStates{}, f.readErr
	}
	out := EndpointDemuxStates{ByENI: map[string][]netip.AddrPort{}, Unowned: f.unowned}
	for _, e := range eniIDs {
		out.ByENI[e] = slices.Clone(f.flows[e])
	}
	return out, nil
}

func (f *fakeDemuxer) EnsureEndpointDemux(_ context.Context, eniID string, entries []netip.AddrPort) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.flows == nil {
		f.flows = map[string][]netip.AddrPort{}
	}
	f.sets = append(f.sets, eniID)
	if len(entries) == 0 {
		delete(f.flows, eniID)
	} else {
		f.flows[eniID] = slices.Clone(entries)
	}
	return nil
}

func (f *fakeDemuxer) of(eni string) []netip.AddrPort {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.flows[eni])
}

var demuxEntries = []netip.AddrPort{
	netip.MustParseAddrPort("192.0.2.11:8443"),
	netip.MustParseAddrPort("192.0.2.10:8443"),
}

func sortedEntries() []netip.AddrPort {
	out := slices.Clone(demuxEntries)
	slices.SortFunc(out, netip.AddrPort.Compare)
	return out
}

// routed returns an intent in which subnets route to an endpoint.
func routed(subnets ...string) func(context.Context) (EndpointIntent, error) {
	return func(context.Context) (EndpointIntent, error) {
		in := EndpointIntent{Subnets: map[string]bool{}, Entries: demuxEntries}
		for _, s := range subnets {
			in.Subnets[s] = true
		}
		return in, nil
	}
}

func demuxManager(d *fakeDemuxer, intent func(context.Context) (EndpointIntent, error)) *Manager {
	return NewManagerWithDeps(Deps{NetworkPlumber: &fakeNetworkPlumber{}, EndpointDemuxer: d, EndpointIntent: intent})
}

// An ovs-vswitchd restart drops the endpoint-cookie flows; the next pass puts
// them back for a guest whose subnet still routes to the endpoint.
func TestReconcileEndpointDemux_ReinstallsLostFlows(t *testing.T) {
	d := &fakeDemuxer{}
	m := demuxManager(d, routed("subnet-0abc"))
	m.Insert(imdsTestVM("i-a", "eni-a", StateRunning))

	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := d.of("eni-a"); !slices.Equal(got, sortedEntries()) {
		t.Fatalf("flows = %v, want %v", got, sortedEntries())
	}
	if !m.EndpointDemuxHeld() {
		t.Error("a node with an installed demux reports none")
	}
}

// A guest whose subnet no longer routes to an endpoint loses its demux, and
// the node then reports holding none.
func TestReconcileEndpointDemux_RemovesStaleDemux(t *testing.T) {
	d := &fakeDemuxer{flows: map[string][]netip.AddrPort{"eni-a": sortedEntries()}}
	m := demuxManager(d, routed("subnet-other"))
	m.Insert(imdsTestVM("i-a", "eni-a", StateRunning))

	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := d.of("eni-a"); len(got) != 0 {
		t.Fatalf("stale demux left: %v", got)
	}
	if m.EndpointDemuxHeld() {
		t.Error("node reports a demux after removing its last")
	}
}

// A matching demux is not rewritten, so a pass never disturbs live traffic.
func TestReconcileEndpointDemux_LeavesMatchingDemux(t *testing.T) {
	d := &fakeDemuxer{flows: map[string][]netip.AddrPort{"eni-a": sortedEntries()}}
	m := demuxManager(d, routed("subnet-0abc"))
	m.Insert(imdsTestVM("i-a", "eni-a", StateRunning))

	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.sets) != 0 {
		t.Errorf("rewrote a matching demux: %v", d.sets)
	}
}

// A guest that starts in a subnet with an existing endpoint gets its demux on
// the next pass.
func TestReconcileEndpointDemux_StartingGuestGetsDemux(t *testing.T) {
	d := &fakeDemuxer{}
	m := demuxManager(d, routed("subnet-0abc"))
	m.Insert(imdsTestVM("i-a", "eni-a", StatePending))

	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.sets) != 0 {
		t.Fatalf("demuxed a guest still launching: %v", d.sets)
	}
	m.UpdateState("i-a", func(v *VM) { v.Status = StateRunning })
	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := d.of("eni-a"); !slices.Equal(got, sortedEntries()) {
		t.Fatalf("flows = %v, want %v", got, sortedEntries())
	}
}

// A teardown holding the ENI lock while a demux install is queued leaves no
// flow: the install re-checks the guest under the lock and skips it.
func TestReconcileEndpointDemux_QueuedInstallYieldsToTeardown(t *testing.T) {
	d := &fakeDemuxer{}
	m := demuxManager(d, routed("subnet-0abc"))
	m.Insert(imdsTestVM("i-a", "eni-a", StateRunning))

	unlock := m.imdsLocks.lock("eni-a")
	done := make(chan error, 1)
	go func() { done <- m.ReconcileEndpointDemux(context.Background()) }()

	select {
	case err := <-done:
		t.Fatalf("pass finished while the ENI lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	m.UpdateState("i-a", func(v *VM) { v.Status = StateStopping })
	m.Delete("i-a")
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(d.sets) != 0 || len(d.of("eni-a")) != 0 {
		t.Errorf("demux installed for a guest torn down: sets %v, flows %v", d.sets, d.of("eni-a"))
	}
}

// A failed read changes nothing and leaves the node reporting a demux, so the
// rollback drain cannot pass on a guess.
func TestReconcileEndpointDemux_UnreadableStateCountsAsHeld(t *testing.T) {
	cases := map[string]struct {
		d      *fakeDemuxer
		intent func(context.Context) (EndpointIntent, error)
	}{
		"bridge": {&fakeDemuxer{readErr: errors.New("ovs down")}, routed()},
		"intent": {&fakeDemuxer{}, func(context.Context) (EndpointIntent, error) {
			return EndpointIntent{}, errors.New("kv down")
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := demuxManager(tc.d, tc.intent)
			m.Insert(imdsTestVM("i-a", "eni-a", StateRunning))
			if err := m.ReconcileEndpointDemux(context.Background()); err == nil {
				t.Fatal("pass reported success without reading")
			}
			if len(tc.d.sets) != 0 {
				t.Errorf("changed flows without a read: %v", tc.d.sets)
			}
			if !m.EndpointDemuxHeld() {
				t.Error("node reports no demux after an unreadable pass")
			}
		})
	}
}

// A fresh process may inherit flows, and a flow no running guest owns is still
// on the bridge: both count until a pass shows none.
func TestEndpointDemuxHeld_UntilAPassShowsNone(t *testing.T) {
	d := &fakeDemuxer{unowned: 1}
	m := demuxManager(d, routed())
	if !m.EndpointDemuxHeld() {
		t.Fatal("a process that has not read the bridge reports no demux")
	}
	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.EndpointDemuxHeld() {
		t.Error("an unowned flow does not count as held")
	}
	d.unowned = 0
	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.EndpointDemuxHeld() {
		t.Error("node with no flow reports a demux")
	}
}
