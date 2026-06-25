package vm

import (
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
)

func TestENISlotFromDeviceID(t *testing.T) {
	tests := []struct {
		in       string
		wantSlot int
		wantOK   bool
	}{
		{"net-eni-1", 1, true},
		{"net-eni-12", 12, true},
		{"net-eni-0", 0, false},     // slot 0 is the non-hotplug sentinel
		{"net-eni-x", 0, false},     // non-numeric
		{"net-eni-", 0, false},      // empty index
		{"virtio-net-0", 0, false},  // wrong prefix
		{"hostnet-eni-1", 0, false}, // netdev, not device
		{"", 0, false},
	}
	for _, tt := range tests {
		slot, ok := eniSlotFromDeviceID(tt.in)
		if slot != tt.wantSlot || ok != tt.wantOK {
			t.Errorf("eniSlotFromDeviceID(%q) = (%d, %v), want (%d, %v)", tt.in, slot, ok, tt.wantSlot, tt.wantOK)
		}
	}
}

func TestENIObjectIDFormats(t *testing.T) {
	if got := eniDeviceID(3); got != "net-eni-3" {
		t.Errorf("eniDeviceID(3) = %q", got)
	}
	if got := eniNetdevID(3); got != "hostnet-eni-3" {
		t.Errorf("eniNetdevID(3) = %q", got)
	}
	if got := eniBusID(3); got != "hotplug-eni3" {
		t.Errorf("eniBusID(3) = %q", got)
	}
}

func TestAdoptENISlot(t *testing.T) {
	_, v, _ := newHotPlugTestVM(t, 4)

	m := &Manager{}
	m.AdoptENISlot(v, "eni-a", 2)
	if got := v.ENIRequests.AttachedByENIID["eni-a"]; got != 2 {
		t.Fatalf("AttachedByENIID[eni-a] = %d, want 2", got)
	}
	if slices.Contains(v.ENIRequests.AvailableSlots, 2) {
		t.Errorf("slot 2 still in free-list after adopt: %v", v.ENIRequests.AvailableSlots)
	}

	// Idempotent: re-adopting the same slot leaves the free-list unchanged.
	before := slices.Clone(v.ENIRequests.AvailableSlots)
	m.AdoptENISlot(v, "eni-a", 2)
	if !slices.Equal(before, v.ENIRequests.AvailableSlots) {
		t.Errorf("re-adopt mutated free-list: %v -> %v", before, v.ENIRequests.AvailableSlots)
	}

	// Nil instance is a no-op (no panic).
	m.AdoptENISlot(nil, "eni-x", 1)
}

func TestAdoptENISlot_NilMap(t *testing.T) {
	v := &VM{ENIRequests: types.ENIRequests{AvailableSlots: []int{1}}}
	m := &Manager{}
	m.AdoptENISlot(v, "eni-a", 1)
	if got := v.ENIRequests.AttachedByENIID["eni-a"]; got != 1 {
		t.Fatalf("AttachedByENIID[eni-a] = %d, want 1", got)
	}
}

func TestReleaseENISlot(t *testing.T) {
	_, v, _ := newHotPlugTestVM(t, 4)
	m := &Manager{}
	m.AdoptENISlot(v, "eni-a", 2)

	m.ReleaseENISlot(v, "eni-a")
	if _, ok := v.ENIRequests.AttachedByENIID["eni-a"]; ok {
		t.Errorf("eni-a still mapped after release")
	}
	if !slices.Contains(v.ENIRequests.AvailableSlots, 2) {
		t.Errorf("slot 2 not returned to free-list: %v", v.ENIRequests.AvailableSlots)
	}

	// Releasing an unknown ENI is a no-op.
	before := slices.Clone(v.ENIRequests.AvailableSlots)
	m.ReleaseENISlot(v, "eni-unknown")
	if !slices.Equal(before, v.ENIRequests.AvailableSlots) {
		t.Errorf("release of unknown ENI mutated free-list")
	}
	m.ReleaseENISlot(nil, "eni-x") // no panic
}

func TestENISlotForReconcileAndMapKeys(t *testing.T) {
	_, v, _ := newHotPlugTestVM(t, 4)
	m := &Manager{}
	m.AdoptENISlot(v, "eni-a", 1)
	m.AdoptENISlot(v, "eni-b", 2)

	if got := m.ENISlotForReconcile(v, "eni-a"); got != 1 {
		t.Errorf("ENISlotForReconcile(eni-a) = %d, want 1", got)
	}
	if got := m.ENISlotForReconcile(v, "eni-missing"); got != 0 {
		t.Errorf("ENISlotForReconcile(eni-missing) = %d, want 0", got)
	}
	if got := m.ENISlotForReconcile(nil, "eni-a"); got != 0 {
		t.Errorf("ENISlotForReconcile(nil) = %d, want 0", got)
	}

	keys := m.ENISlotMapKeys(v)
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"eni-a", "eni-b"}) {
		t.Errorf("ENISlotMapKeys = %v, want [eni-a eni-b]", keys)
	}
	if got := m.ENISlotMapKeys(nil); got != nil {
		t.Errorf("ENISlotMapKeys(nil) = %v, want nil", got)
	}
}

func TestListLiveENIDevices(t *testing.T) {
	mgr, v, stub := newHotPlugTestVM(t, 4)
	// Two ENI hot-plug devices + one unrelated device.
	mustAdd(t, stub, "net-eni-1")
	mustAdd(t, stub, "net-eni-3")
	mustAdd(t, stub, "virtio-blk-0")

	live, err := mgr.ListLiveENIDevices(v)
	if err != nil {
		t.Fatalf("ListLiveENIDevices: %v", err)
	}
	if len(live) != 2 || live[1] != "net-eni-1" || live[3] != "net-eni-3" {
		t.Errorf("live = %v, want {1:net-eni-1, 3:net-eni-3}", live)
	}
}

func TestListLiveENIDevices_NoQMP(t *testing.T) {
	mgr := NewManagerWithDeps(Deps{})
	v := &VM{ID: "i-noqmp"}
	if _, err := mgr.ListLiveENIDevices(v); err == nil {
		t.Fatalf("expected error for nil QMPClient")
	}
}

func TestRemoveENIDeviceBySlot(t *testing.T) {
	mgr, v, stub := newHotPlugTestVM(t, 4)
	mustAdd(t, stub, "net-eni-2")
	if err := stub.NetdevAdd(map[string]any{"id": "hostnet-eni-2"}); err != nil {
		t.Fatalf("seed netdev: %v", err)
	}

	if err := mgr.RemoveENIDeviceBySlot(v, 2); err != nil {
		t.Fatalf("RemoveENIDeviceBySlot: %v", err)
	}
	if stub.HasDevice("net-eni-2") {
		t.Errorf("device net-eni-2 still present after removal")
	}
	if stub.HasNetdev("hostnet-eni-2") {
		t.Errorf("netdev hostnet-eni-2 still present after removal")
	}
}

func TestRemoveENIDeviceBySlot_NoQMP(t *testing.T) {
	mgr := NewManagerWithDeps(Deps{})
	v := &VM{ID: "i-noqmp"}
	if err := mgr.RemoveENIDeviceBySlot(v, 1); err == nil {
		t.Fatalf("expected error for nil QMPClient")
	}
	if err := mgr.RemoveENIDeviceBySlot(nil, 1); err == nil {
		t.Fatalf("expected error for nil instance")
	}
}

func TestRemoveENIDeviceBySlot_DeviceDelFails(t *testing.T) {
	mgr, v, stub := newHotPlugTestVM(t, 4)
	// No device seeded → stub device_del returns "not found".
	if err := mgr.RemoveENIDeviceBySlot(v, 9); err == nil {
		t.Fatalf("expected device_del error for absent device")
	}
	_ = stub
}

func TestCleanupENITap(t *testing.T) {
	_, v, _, plumber := newHotPlugTestVMWithPlumber(t, 4)
	mgr := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	mgr.Insert(v)

	mgr.CleanupENITap(v, "eni-a")
	want := TapDeviceName("eni-a")
	if len(plumber.cleanupCalls) != 1 || plumber.cleanupCalls[0] != want {
		t.Errorf("cleanupCalls = %v, want [%s]", plumber.cleanupCalls, want)
	}
	mgr.CleanupENITap(nil, "eni-x") // no panic
}

func TestInstanceIDHelper(t *testing.T) {
	if got := instanceID(nil); got != "<nil>" {
		t.Errorf("instanceID(nil) = %q, want <nil>", got)
	}
	if got := instanceID(&VM{ID: "i-1"}); got != "i-1" {
		t.Errorf("instanceID(i-1) = %q", got)
	}
}

func mustAdd(t *testing.T, stub *StubDeviceController, id string) {
	t.Helper()
	if err := stub.DeviceAdd(map[string]any{"id": id}); err != nil {
		t.Fatalf("seed device %s: %v", id, err)
	}
}

// keep qmp import used when QMPClient literal is needed in future cases.
var _ = qmp.QMPClient{}
