package vm

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

func imdsTestVM(id, eni string, status InstanceState) *VM {
	return &VM{
		ID:       id,
		Status:   status,
		ENIId:    eni,
		ENIMac:   "02:00:00:00:00:01",
		Instance: &ec2.Instance{SubnetId: aws.String("subnet-0abc")},
	}
}

var (
	imdsIntact  = IMDSDatapathState{FlowsFound: 7, FlowsExpected: 7, EndpointReady: true}
	imdsFlushed = IMDSDatapathState{FlowsFound: 0, FlowsExpected: 7, EndpointReady: true}
)

func TestIMDSTargetOf(t *testing.T) {
	noSubnet := imdsTestVM("i-nosubnet", "eni-1", StateRunning)
	noSubnet.Instance.SubnetId = nil
	noInstance := imdsTestVM("i-noinstance", "eni-2", StateRunning)
	noInstance.Instance = nil

	cases := []struct {
		name string
		v    *VM
		want bool
	}{
		{"running with ENI and subnet", imdsTestVM("i-run", "eni-0", StateRunning), true},
		{"stopping", imdsTestVM("i-stop", "eni-0", StateStopping), false},
		{"error after a crash", imdsTestVM("i-err", "eni-0", StateError), false},
		{"pending mid-launch", imdsTestVM("i-pend", "eni-0", StatePending), false},
		{"no primary ENI", imdsTestVM("i-noeni", "", StateRunning), false},
		{"no subnet", noSubnet, false},
		{"no instance record", noInstance, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := imdsTargetOf(tc.v); got != tc.want {
				t.Errorf("imdsTargetOf = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIMDSDatapathsToRepair(t *testing.T) {
	targets := []imdsDatapathTarget{
		{instanceID: "i-intact", eniID: "eni-intact"},
		{instanceID: "i-partial", eniID: "eni-partial"},
		{instanceID: "i-flushed", eniID: "eni-flushed"},
		{instanceID: "i-reloaded", eniID: "eni-reloaded"},
		{instanceID: "i-unread", eniID: "eni-unread"},
	}
	states := map[string]IMDSDatapathState{
		"eni-intact":   imdsIntact,
		"eni-partial":  {FlowsFound: 3, FlowsExpected: 7, EndpointReady: true},
		"eni-flushed":  imdsFlushed,
		"eni-reloaded": {FlowsFound: 7, FlowsExpected: 7, EndpointReady: false},
	}
	got := imdsDatapathsToRepair(targets, states)
	var ids []string
	for _, t := range got {
		ids = append(ids, t.eniID)
	}
	want := []string{"eni-partial", "eni-flushed", "eni-reloaded"}
	if len(ids) != len(want) {
		t.Fatalf("repair set = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("repair set = %v, want %v", ids, want)
		}
	}
}

// A re-install starts by clearing the tap's flows, so repairing an intact
// datapath would cut a working guest's network every pass.
func TestReconcileIMDSDatapaths_LeavesIntactGuestAlone(t *testing.T) {
	plumber := &fakeNetworkPlumber{imdsStates: map[string]IMDSDatapathState{"eni-ok": imdsIntact}}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	m.Insert(imdsTestVM("i-ok", "eni-ok", StateRunning))

	m.reconcileIMDSDatapaths()

	if len(plumber.imdsRepairCalls) != 0 {
		t.Errorf("intact datapath was repaired: %+v", plumber.imdsRepairCalls)
	}
}

func TestReconcileIMDSDatapaths_RepairsLostDatapath(t *testing.T) {
	plumber := &fakeNetworkPlumber{imdsStates: map[string]IMDSDatapathState{
		"eni-ok":   imdsIntact,
		"eni-lost": imdsFlushed,
		"eni-stop": imdsFlushed,
	}}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	m.Insert(imdsTestVM("i-ok", "eni-ok", StateRunning))
	lost := imdsTestVM("i-lost", "eni-lost", StateRunning)
	lost.ENIMac = "02:00:00:00:00:99"
	lost.Instance.SubnetId = aws.String("subnet-0lost")
	m.Insert(lost)
	m.Insert(imdsTestVM("i-stop", "eni-stop", StateStopped))

	m.reconcileIMDSDatapaths()

	want := []imdsAttachCall{{eniID: "eni-lost", mac: "02:00:00:00:00:99", subnetID: "subnet-0lost"}}
	if len(plumber.imdsRepairCalls) != 1 || plumber.imdsRepairCalls[0] != want[0] {
		t.Errorf("repair calls = %+v, want %+v", plumber.imdsRepairCalls, want)
	}
}

// A failed read says nothing about the datapaths, so it must not be taken as
// every guest having lost its flows.
func TestReconcileIMDSDatapaths_ReadFailureRepairsNothing(t *testing.T) {
	plumber := &fakeNetworkPlumber{imdsStatesErr: errors.New("br-imds.mgmt: connection refused")}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	m.Insert(imdsTestVM("i-a", "eni-a", StateRunning))

	m.reconcileIMDSDatapaths()

	if len(plumber.imdsRepairCalls) != 0 {
		t.Errorf("read failure triggered repairs: %+v", plumber.imdsRepairCalls)
	}
}

// The pass reads its targets before it repairs them; a guest stopped or gone
// in between must not have its datapath re-created.
func TestRepairIMDSDatapath_RechecksGuestUnderLock(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m *Manager)
	}{
		{"stopped", func(m *Manager) { m.UpdateState("i-a", func(v *VM) { v.Status = StateStopping }) }},
		{"removed", func(m *Manager) { m.Delete("i-a") }},
		{"ENI replaced", func(m *Manager) { m.UpdateState("i-a", func(v *VM) { v.ENIId = "eni-b" }) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plumber := &fakeNetworkPlumber{}
			m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
			v := imdsTestVM("i-a", "eni-a", StateRunning)
			m.Insert(v)
			target, _ := imdsTargetOf(v)

			tc.mutate(m)
			m.repairIMDSDatapath(target, imdsFlushed)

			if len(plumber.imdsRepairCalls) != 0 {
				t.Errorf("repaired a guest no longer running with that ENI: %+v", plumber.imdsRepairCalls)
			}
		})
	}
}

// Teardown holds the ENI lock across detach and tap cleanup; a repair must
// wait for it rather than re-create what the teardown is removing.
func TestRepairIMDSDatapath_WaitsForTeardownLock(t *testing.T) {
	plumber := &fakeNetworkPlumber{}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	v := imdsTestVM("i-a", "eni-a", StateRunning)
	m.Insert(v)
	target, _ := imdsTargetOf(v)

	unlock := m.imdsLocks.lock("eni-a")
	done := make(chan struct{})
	go func() {
		m.repairIMDSDatapath(target, imdsFlushed)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("repair ran while the ENI lock was held")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	<-done
	if len(plumber.imdsRepairCalls) != 1 {
		t.Errorf("repair calls after unlock = %d, want 1", len(plumber.imdsRepairCalls))
	}
}

// A repair landing between detach and tap cleanup would find the tap still on
// br-imds and re-create the endpoint and patch of a guest being torn down.
func TestCleanupTapDevices_HoldsENILockAcrossDetachAndCleanup(t *testing.T) {
	inDetach, releaseDetach := make(chan struct{}), make(chan struct{})
	inCleanup, releaseCleanup := make(chan struct{}), make(chan struct{})
	plumber := &fakeNetworkPlumber{
		detachHook:  func() { close(inDetach); <-releaseDetach },
		cleanupHook: func() { close(inCleanup); <-releaseCleanup },
	}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	v := imdsTestVM("i-a", "eni-a", StateRunning)
	m.Insert(v)
	target, _ := imdsTargetOf(v)

	var wg sync.WaitGroup
	wg.Go(func() { m.cleanupTapDevices(v) })
	<-inDetach
	wg.Go(func() { m.repairIMDSDatapath(target, imdsFlushed) })
	for _, step := range []struct {
		name    string
		release chan struct{}
		next    chan struct{}
	}{{"detach", releaseDetach, inCleanup}, {"tap cleanup", releaseCleanup, nil}} {
		time.Sleep(50 * time.Millisecond)
		if n := plumber.repairCount(); n != 0 {
			t.Fatalf("repair ran during %s: %d calls", step.name, n)
		}
		close(step.release)
		if step.next != nil {
			<-step.next
		}
	}
	wg.Wait()

	want := []string{"detach:eni-a", "cleanup:" + TapDeviceName("eni-a"), "repair:eni-a"}
	if !slices.Equal(plumber.order, want) {
		t.Errorf("call order = %v, want %v", plumber.order, want)
	}
}

// An ovs-vswitchd restart drifts every guest at once; repairs must overlap so
// the last guest is not dark for the sum of everyone else's install.
func TestReconcileIMDSDatapaths_RepairsConcurrentlyUpToLimit(t *testing.T) {
	const guests = imdsRepairConcurrency + 4
	var inFlight atomic.Int32
	var overLimit atomic.Bool
	full, release := make(chan struct{}), make(chan struct{})
	var fullOnce sync.Once
	plumber := &fakeNetworkPlumber{
		imdsStates: map[string]IMDSDatapathState{},
		repairHook: func() {
			n := inFlight.Add(1)
			if n > imdsRepairConcurrency {
				overLimit.Store(true)
			}
			if n == imdsRepairConcurrency {
				fullOnce.Do(func() { close(full) })
			}
			<-release
			inFlight.Add(-1)
		},
	}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	for i := range guests {
		eni := fmt.Sprintf("eni-%d", i)
		plumber.imdsStates[eni] = imdsFlushed
		m.Insert(imdsTestVM(fmt.Sprintf("i-%d", i), eni, StateRunning))
	}

	done := make(chan struct{})
	go func() {
		m.reconcileIMDSDatapaths()
		close(done)
	}()
	select {
	case <-full:
	case <-time.After(5 * time.Second):
		t.Fatalf("repairs did not reach %d in flight: %d", imdsRepairConcurrency, inFlight.Load())
	}
	time.Sleep(50 * time.Millisecond)
	if overLimit.Load() {
		t.Errorf("more than %d repairs ran at once", imdsRepairConcurrency)
	}
	close(release)
	<-done
	if n := plumber.repairCount(); n != guests {
		t.Errorf("repairs = %d, want %d", n, guests)
	}
}

func TestRepairIMDSDatapath_DetachedTapIsNotAnError(t *testing.T) {
	plumber := &fakeNetworkPlumber{imdsRepairErr: ErrIMDSTapDetached}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	v := imdsTestVM("i-a", "eni-a", StateRunning)
	m.Insert(v)
	target, _ := imdsTargetOf(v)

	m.repairIMDSDatapath(target, imdsFlushed)

	if len(plumber.imdsRepairCalls) != 1 {
		t.Errorf("repair calls = %d, want 1", len(plumber.imdsRepairCalls))
	}
}

func TestKeyedMutex_DropsUnheldKeys(t *testing.T) {
	var k keyedMutex
	unlockA := k.lock("a")
	unlockB := k.lock("b")
	unlockA()
	unlockB()
	if len(k.locks) != 0 {
		t.Errorf("unheld keys retained: %d", len(k.locks))
	}
}
