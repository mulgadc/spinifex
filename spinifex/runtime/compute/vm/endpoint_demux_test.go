package vm

import (
	"context"
	"errors"
	"fmt"
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
	unowned []uint64
	rmErr   error
	readErr error
	setErr  error
	sets    []string
	reads   int
}

var _ EndpointDemuxer = (*fakeDemuxer)(nil)

func (f *fakeDemuxer) EndpointDemuxStates(_ context.Context, eniIDs []string) (EndpointDemuxStates, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.readErr != nil {
		return EndpointDemuxStates{}, f.readErr
	}
	out := EndpointDemuxStates{ByENI: map[string][]netip.AddrPort{}, Unowned: slices.Clone(f.unowned)}
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
	if f.setErr != nil {
		return f.setErr
	}
	if len(entries) == 0 {
		delete(f.flows, eniID)
	} else {
		f.flows[eniID] = slices.Clone(entries)
	}
	return nil
}

func (f *fakeDemuxer) RemoveEndpointDemuxCookie(_ context.Context, cookie uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rmErr != nil {
		return f.rmErr
	}
	f.unowned = slices.DeleteFunc(f.unowned, func(c uint64) bool { return c == cookie })
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

// A fresh process may inherit flows, so it reports them held until a pass
// shows none. A flow no running guest owns is removed; until it is, it holds.
func TestEndpointDemuxHeld_UntilAPassShowsNone(t *testing.T) {
	d := &fakeDemuxer{unowned: []uint64{0xa1e500000001}, rmErr: errors.New("ovs down")}
	m := demuxManager(d, routed())
	if !m.EndpointDemuxHeld() {
		t.Fatal("a process that has not read the bridge reports no demux")
	}
	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.EndpointDemuxHeld() {
		t.Error("an unowned flow that could not be removed does not count as held")
	}
	d.rmErr = nil
	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.unowned) != 0 {
		t.Errorf("unowned flows left: %#x", d.unowned)
	}
	if m.EndpointDemuxHeld() {
		t.Error("node with no flow left reports a demux")
	}
}

// A pass that read intent before an endpoint was deleted may still install its
// flows, so a clear node reads held from the moment a pass starts until it ends.
func TestEndpointDemuxHeld_WhileAPassRuns(t *testing.T) {
	read, release := make(chan struct{}), make(chan struct{})
	m := demuxManager(&fakeDemuxer{}, func(context.Context) (EndpointIntent, error) {
		close(read)
		<-release
		return EndpointIntent{}, nil
	})
	m.Insert(imdsTestVM("i-a", "eni-a", StateRunning))
	m.endpointDemux.clear.Store(true)
	done := make(chan error, 1)
	go func() { done <- m.ReconcileEndpointDemux(context.Background()) }()
	<-read
	if !m.EndpointDemuxHeld() {
		t.Error("a node whose pass is still running reports no demux")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if m.EndpointDemuxHeld() {
		t.Error("node with no flow left reports a demux")
	}
}

// A pass that leaves some taps alone and rewrites others reports the node as
// holding flows, whichever finishes first.
func TestReconcileEndpointDemux_MixedPassReportsHeld(t *testing.T) {
	d := &fakeDemuxer{flows: map[string][]netip.AddrPort{}}
	m := demuxManager(d, routed("subnet-0abc"))
	for i := range 16 {
		eni := fmt.Sprintf("eni-%02d", i)
		m.Insert(imdsTestVM(fmt.Sprintf("i-%02d", i), eni, StateRunning))
		if i%2 == 0 {
			d.flows[eni] = sortedEntries()
		} else {
			d.flows[eni] = demuxEntries[:1]
		}
	}
	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.sets) != 8 {
		t.Errorf("rewrote %d taps, want the 8 that differ", len(d.sets))
	}
	if !m.EndpointDemuxHeld() {
		t.Error("node with installed flows reports none")
	}
}

// A demux update that fails may have left the tap's flows partly written, so
// the node keeps reporting them held whether it was installing or removing.
func TestReconcileEndpointDemux_FailedUpdateCountsAsHeld(t *testing.T) {
	cases := map[string]struct {
		flows  map[string][]netip.AddrPort
		routes []string
	}{
		"removal":      {flows: map[string][]netip.AddrPort{"eni-a": sortedEntries()}, routes: []string{"subnet-other"}},
		"installation": {flows: map[string][]netip.AddrPort{}, routes: []string{"subnet-0abc"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := &fakeDemuxer{flows: tc.flows, setErr: errors.New("ovs-ofctl failed")}
			m := demuxManager(d, routed(tc.routes...))
			m.Insert(imdsTestVM("i-a", "eni-a", StateRunning))
			if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(d.sets) != 1 {
				t.Fatalf("attempted %d updates, want 1", len(d.sets))
			}
			if !m.EndpointDemuxHeld() {
				t.Error("a node whose demux update failed reports no flows")
			}
		})
	}
}

// counted wraps intent and counts its reads.
func counted(intent func(context.Context) (EndpointIntent, error), n *int) func(context.Context) (EndpointIntent, error) {
	return func(ctx context.Context) (EndpointIntent, error) {
		*n++
		return intent(ctx)
	}
}

// An idle node reads nothing once a pass has shown no flow, and a guest that
// starts after a skipped pass gets its demux from the next pass.
func TestReconcileEndpointDemux_IdleNodeSkipsReads(t *testing.T) {
	d := &fakeDemuxer{}
	intents := 0
	m := demuxManager(d, counted(routed("subnet-0abc"), &intents))

	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if intents != 1 || d.reads != 1 {
		t.Fatalf("first pass: %d intent reads, %d dumps, want 1 and 1", intents, d.reads)
	}
	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if intents != 1 || d.reads != 1 {
		t.Errorf("idle pass after a clear pass: %d intent reads, %d dumps, want none", intents-1, d.reads-1)
	}
	if m.EndpointDemuxHeld() {
		t.Error("idle node reports a demux")
	}

	m.Insert(imdsTestVM("i-a", "eni-a", StateRunning))
	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := d.of("eni-a"); !slices.Equal(got, sortedEntries()) {
		t.Fatalf("guest after an idle pass: flows = %v, want %v", got, sortedEntries())
	}
}

// A pass that could not remove an unowned flow leaves the node held, so the
// next idle pass reads and retries.
func TestReconcileEndpointDemux_IdleNodeRetriesUnownedFlow(t *testing.T) {
	d := &fakeDemuxer{unowned: []uint64{0xa1e500000001}, rmErr: errors.New("ovs down")}
	intents := 0
	m := demuxManager(d, counted(routed(), &intents))
	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.ReconcileEndpointDemux(context.Background()); err != nil {
		t.Fatal(err)
	}
	if intents != 2 || d.reads != 2 {
		t.Errorf("pass after a failed removal: %d intent reads, %d dumps, want 1 and 1", intents-1, d.reads-1)
	}
}
