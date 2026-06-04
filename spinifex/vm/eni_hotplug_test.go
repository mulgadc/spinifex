package vm

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
)

// newHotPlugTestVM returns a VM in StateRunning with a non-nil QMPClient
// and a pre-populated ENIRequests allocator. The stub controller is
// substituted via the newDeviceController seam for the duration of the
// test (cleanup restores the production factory).
func newHotPlugTestVM(t *testing.T, slots int) (*Manager, *VM, *StubDeviceController) {
	t.Helper()
	mgr, v, stub, _ := newHotPlugTestVMWithPlumber(t, slots)
	return mgr, v, stub
}

// newHotPlugTestVMWithPlumber is the variant that exposes the fake
// NetworkPlumber so 3c tests can assert tap/OVS SetupTap / CleanupTap calls.
func newHotPlugTestVMWithPlumber(t *testing.T, slots int) (*Manager, *VM, *StubDeviceController, *fakeNetworkPlumber) {
	t.Helper()
	plumber := &fakeNetworkPlumber{}
	mgr := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	stub := NewStubDeviceController()
	available := make([]int, 0, slots)
	for i := 1; i <= slots; i++ {
		available = append(available, i)
	}
	v := &VM{
		ID:        "i-test",
		Status:    StateRunning,
		QMPClient: &qmp.QMPClient{},
		ENIRequests: types.ENIRequests{
			AvailableSlots:  available,
			AttachedByENIID: make(map[string]int),
		},
	}
	mgr.Insert(v)

	origFactory := newDeviceController
	newDeviceController = func(*VM) DeviceController { return stub }
	origSleep := eniPipelineSleep
	eniPipelineSleep = func(time.Duration) {}
	t.Cleanup(func() {
		newDeviceController = origFactory
		eniPipelineSleep = origSleep
	})
	return mgr, v, stub, plumber
}

func TestHotPlugENI_SuccessPath(t *testing.T) {
	mgr, v, stub := newHotPlugTestVM(t, 2)

	res, err := mgr.HotPlugENI(v, "eni-aaaa", "02:00:00:00:00:01")
	if err != nil {
		t.Fatalf("HotPlugENI: %v", err)
	}
	if res.Slot != 1 {
		t.Errorf("slot = %d, want 1", res.Slot)
	}
	if !stub.HasDevice("net-eni-1") {
		t.Errorf("device net-eni-1 not attached in stub")
	}
	if !stub.HasNetdev("hostnet-eni-1") {
		t.Errorf("netdev hostnet-eni-1 not attached in stub")
	}
	if got, ok := v.ENIRequests.AttachedByENIID["eni-aaaa"]; !ok || got != 1 {
		t.Errorf("AttachedByENIID[eni-aaaa] = %v ok=%v, want 1 true", got, ok)
	}
	if len(v.ENIRequests.AvailableSlots) != 1 || v.ENIRequests.AvailableSlots[0] != 2 {
		t.Errorf("AvailableSlots = %v, want [2]", v.ENIRequests.AvailableSlots)
	}
}

func TestHotPlugENI_CallOrder(t *testing.T) {
	mgr, v, stub := newHotPlugTestVM(t, 1)

	if _, err := mgr.HotPlugENI(v, "eni-aaaa", "02:00:00:00:00:01"); err != nil {
		t.Fatalf("HotPlugENI: %v", err)
	}
	calls := stub.Calls()
	want := []string{"netdev_add", "device_add", "query-pci"}
	if len(calls) < len(want) {
		t.Fatalf("call count = %d, want >= %d (%v)", len(calls), len(want), calls)
	}
	for i, w := range want {
		if calls[i].Execute != w {
			t.Errorf("call[%d] = %s, want %s", i, calls[i].Execute, w)
		}
	}
}

func TestHotPlugENI_DeviceAddFailureRollsBack(t *testing.T) {
	mgr, v, stub := newHotPlugTestVM(t, 2)
	wantErr := errors.New("device_add boom")
	stub.SetFailNext("device_add", wantErr)

	_, err := mgr.HotPlugENI(v, "eni-aaaa", "02:00:00:00:00:01")
	if err == nil || !strings.Contains(err.Error(), "device_add") {
		t.Fatalf("HotPlugENI err = %v, want device_add failure", err)
	}
	if stub.HasDevice("net-eni-1") {
		t.Errorf("device net-eni-1 left attached after rollback")
	}
	if stub.HasNetdev("hostnet-eni-1") {
		t.Errorf("netdev hostnet-eni-1 left attached after rollback")
	}
	if _, ok := v.ENIRequests.AttachedByENIID["eni-aaaa"]; ok {
		t.Errorf("AttachedByENIID still tracks eni-aaaa after rollback")
	}
	if !containsSlot(v.ENIRequests.AvailableSlots, 1) {
		t.Errorf("slot 1 not returned to free-list: %v", v.ENIRequests.AvailableSlots)
	}
}

func TestHotPlugENI_NetdevAddFailureRollsBack(t *testing.T) {
	mgr, v, stub := newHotPlugTestVM(t, 1)
	stub.SetFailNext("netdev_add", errors.New("netdev boom"))

	_, err := mgr.HotPlugENI(v, "eni-aaaa", "02:00:00:00:00:01")
	if err == nil {
		t.Fatalf("HotPlugENI: want error, got nil")
	}
	if stub.HasNetdev("hostnet-eni-1") {
		t.Errorf("netdev should not exist after netdev_add failure")
	}
	if !containsSlot(v.ENIRequests.AvailableSlots, 1) {
		t.Errorf("slot 1 not returned to free-list")
	}
}

func TestHotPlugENI_AttachmentLimitExceeded(t *testing.T) {
	mgr, v, _ := newHotPlugTestVM(t, 0)
	_, err := mgr.HotPlugENI(v, "eni-aaaa", "02:00:00:00:00:01")
	if !errors.Is(err, ErrAttachmentLimitExceeded) {
		t.Fatalf("err = %v, want ErrAttachmentLimitExceeded", err)
	}
}

func TestHotPlugENI_IdempotentReattach(t *testing.T) {
	mgr, v, stub := newHotPlugTestVM(t, 2)

	res1, err := mgr.HotPlugENI(v, "eni-aaaa", "02:00:00:00:00:01")
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	callsBefore := len(stub.Calls())
	res2, err := mgr.HotPlugENI(v, "eni-aaaa", "02:00:00:00:00:01")
	if err != nil {
		t.Fatalf("idempotent reattach: %v", err)
	}
	if res1.Slot != res2.Slot {
		t.Errorf("idempotent slot drift: %d vs %d", res1.Slot, res2.Slot)
	}
	if got := len(stub.Calls()); got != callsBefore {
		t.Errorf("second attach issued %d new calls, want 0", got-callsBefore)
	}
}

func TestHotPlugENI_RejectsNonRunning(t *testing.T) {
	mgr, v, _ := newHotPlugTestVM(t, 1)
	v.Status = StateStopped
	_, err := mgr.HotPlugENI(v, "eni-aaaa", "02:00:00:00:00:01")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
}

func TestHotPlugENI_RejectsNilQMP(t *testing.T) {
	mgr, v, _ := newHotPlugTestVM(t, 1)
	v.QMPClient = nil
	_, err := mgr.HotPlugENI(v, "eni-aaaa", "02:00:00:00:00:01")
	if !errors.Is(err, ErrQMPUnavailable) {
		t.Fatalf("err = %v, want ErrQMPUnavailable", err)
	}
}

func TestHotUnplugENI_SuccessFreesSlot(t *testing.T) {
	mgr, v, stub := newHotPlugTestVM(t, 2)
	if _, err := mgr.HotPlugENI(v, "eni-aaaa", "02:00:00:00:00:01"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := mgr.HotUnplugENI(v, "eni-aaaa", false); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if stub.HasDevice("net-eni-1") {
		t.Errorf("device should be gone after detach")
	}
	if stub.HasNetdev("hostnet-eni-1") {
		t.Errorf("netdev should be gone after detach")
	}
	if _, ok := v.ENIRequests.AttachedByENIID["eni-aaaa"]; ok {
		t.Errorf("AttachedByENIID still tracks eni-aaaa")
	}
	if !containsSlot(v.ENIRequests.AvailableSlots, 1) {
		t.Errorf("slot 1 not returned to free-list: %v", v.ENIRequests.AvailableSlots)
	}
}

func TestHotUnplugENI_NotAttached(t *testing.T) {
	mgr, v, _ := newHotPlugTestVM(t, 1)
	err := mgr.HotUnplugENI(v, "eni-missing", false)
	if !errors.Is(err, ErrENINotAttached) {
		t.Fatalf("err = %v, want ErrENINotAttached", err)
	}
}

func TestHotUnplugENI_DeviceDelFailureWithoutForce(t *testing.T) {
	mgr, v, stub := newHotPlugTestVM(t, 1)
	if _, err := mgr.HotPlugENI(v, "eni-aaaa", "02:00:00:00:00:01"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	stub.SetFailNext("device_del", errors.New("device_del boom"))

	err := mgr.HotUnplugENI(v, "eni-aaaa", false)
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	if !stub.HasDevice("net-eni-1") {
		t.Errorf("device should still be present after device_del failure (no force)")
	}
}

func TestHotUnplugENI_ForceContinuesPastFailure(t *testing.T) {
	mgr, v, stub := newHotPlugTestVM(t, 1)
	if _, err := mgr.HotPlugENI(v, "eni-aaaa", "02:00:00:00:00:01"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	stub.SetFailNext("device_del", errors.New("device_del boom"))

	// query-pci poll will see device still present until we manually nuke it.
	// Pre-clear so the wait-for-absence loop exits quickly under force.
	stub.mu.Lock()
	delete(stub.devices, "net-eni-1")
	stub.mu.Unlock()

	if err := mgr.HotUnplugENI(v, "eni-aaaa", true); err != nil {
		t.Fatalf("force detach: %v", err)
	}
	if _, ok := v.ENIRequests.AttachedByENIID["eni-aaaa"]; ok {
		t.Errorf("AttachedByENIID still tracks eni-aaaa after force detach")
	}
}

func TestWaitForPCIDevice_TimeoutOnAttach(t *testing.T) {
	stub := NewStubDeviceController()
	origSleep := eniPipelineSleep
	origMax := eniPipeline.AttachPollMax
	eniPipelineSleep = func(time.Duration) {}
	eniPipeline.AttachPollMax = 3
	t.Cleanup(func() {
		eniPipelineSleep = origSleep
		eniPipeline.AttachPollMax = origMax
	})

	err := waitForPCIDevice(stub, "net-eni-99", true)
	if !errors.Is(err, ErrENIPipelineTimeout) {
		t.Fatalf("err = %v, want ErrENIPipelineTimeout", err)
	}
}

func TestHotPlugENI_ConcurrentAttachesSerialize(t *testing.T) {
	mgr, v, _ := newHotPlugTestVM(t, 3)

	var wg sync.WaitGroup
	results := make(chan int, 3)
	errs := make(chan error, 3)
	for i := range 3 {
		wg.Add(1)
		eniID := fmt.Sprintf("eni-%04d", i)
		mac := fmt.Sprintf("02:00:00:00:00:%02d", i+1)
		go func() {
			defer wg.Done()
			res, err := mgr.HotPlugENI(v, eniID, mac)
			if err != nil {
				errs <- err
				return
			}
			results <- res.Slot
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		t.Errorf("concurrent attach failed: %v", e)
	}
	seen := map[int]bool{}
	for s := range results {
		if seen[s] {
			t.Errorf("slot %d returned twice", s)
		}
		seen[s] = true
	}
	if len(seen) != 3 {
		t.Errorf("got %d distinct slots, want 3", len(seen))
	}
}

func TestHotPlugENI_WiresTapOnBrInt(t *testing.T) {
	mgr, v, _, plumber := newHotPlugTestVMWithPlumber(t, 2)

	if _, err := mgr.HotPlugENI(v, "eni-abc123", "02:00:00:aa:bb:cc"); err != nil {
		t.Fatalf("HotPlugENI: %v", err)
	}
	if len(plumber.setupCalls) != 1 {
		t.Fatalf("SetupTap calls = %d, want 1", len(plumber.setupCalls))
	}
	got := plumber.setupCalls[0]
	want := VPCTapSpec("eni-abc123", "02:00:00:aa:bb:cc")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SetupTap spec = %+v, want %+v", got, want)
	}
	if len(plumber.cleanupCalls) != 0 {
		t.Errorf("CleanupTap called %d times on success, want 0", len(plumber.cleanupCalls))
	}
}

func TestHotPlugENI_TapSetupFailureAbortsBeforeQMP(t *testing.T) {
	mgr, v, stub, plumber := newHotPlugTestVMWithPlumber(t, 1)
	plumber.setupErr = errors.New("ovs add-port boom")

	_, err := mgr.HotPlugENI(v, "eni-abc123", "02:00:00:aa:bb:cc")
	if err == nil || !strings.Contains(err.Error(), "tap/ovs setup") {
		t.Fatalf("err = %v, want tap/ovs setup failure", err)
	}
	if len(stub.Calls()) != 0 {
		t.Errorf("QMP calls issued after tap setup failure: %v", stub.Calls())
	}
	if !containsSlot(v.ENIRequests.AvailableSlots, 1) {
		t.Errorf("slot 1 not returned to free-list: %v", v.ENIRequests.AvailableSlots)
	}
}

func TestHotPlugENI_QMPFailureCleansUpTap(t *testing.T) {
	mgr, v, stub, plumber := newHotPlugTestVMWithPlumber(t, 1)
	stub.SetFailNext("netdev_add", errors.New("netdev boom"))

	if _, err := mgr.HotPlugENI(v, "eni-abc123", "02:00:00:aa:bb:cc"); err == nil {
		t.Fatalf("HotPlugENI: want error, got nil")
	}
	wantTap := TapDeviceName("eni-abc123")
	if len(plumber.cleanupCalls) != 1 || plumber.cleanupCalls[0] != wantTap {
		t.Errorf("CleanupTap calls = %v, want [%s]", plumber.cleanupCalls, wantTap)
	}
}

func TestHotUnplugENI_CleansUpTap(t *testing.T) {
	mgr, v, _, plumber := newHotPlugTestVMWithPlumber(t, 1)
	if _, err := mgr.HotPlugENI(v, "eni-abc123", "02:00:00:aa:bb:cc"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := mgr.HotUnplugENI(v, "eni-abc123", false); err != nil {
		t.Fatalf("detach: %v", err)
	}
	wantTap := TapDeviceName("eni-abc123")
	if len(plumber.cleanupCalls) != 1 || plumber.cleanupCalls[0] != wantTap {
		t.Errorf("CleanupTap calls = %v, want [%s]", plumber.cleanupCalls, wantTap)
	}
}

func containsSlot(slots []int, s int) bool {
	return slices.Contains(slots, s)
}
