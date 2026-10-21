package vm

import (
	"errors"
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
