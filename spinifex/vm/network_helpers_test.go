package vm

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeNetworkPlumber records calls so tests can assert per-spec behaviour.
type fakeNetworkPlumber struct {
	setupCalls   []TapSpec
	cleanupCalls []string
	setupErr     error
	cleanupErr   error
}

func (p *fakeNetworkPlumber) SetupTap(spec TapSpec) error {
	p.setupCalls = append(p.setupCalls, spec)
	return p.setupErr
}

func (p *fakeNetworkPlumber) CleanupTap(name string) error {
	p.cleanupCalls = append(p.cleanupCalls, name)
	return p.cleanupErr
}

var _ NetworkPlumber = (*fakeNetworkPlumber)(nil)

func TestMgmtTapName(t *testing.T) {
	tests := []struct {
		instanceID string
		want       string
	}{
		{"i-abc123", "mgabc123"},
		{"i-abc123def456789", "mgabc123def4567"}, // truncated to 15 chars
		{"i-a", "mga"},
		{"abc123", "mgabc123"}, // no i- prefix
	}
	for _, tt := range tests {
		t.Run(tt.instanceID, func(t *testing.T) {
			got := MgmtTapName(tt.instanceID)
			if got != tt.want {
				t.Errorf("MgmtTapName(%q) = %q, want %q", tt.instanceID, got, tt.want)
			}
			if len(got) > 15 {
				t.Errorf("MgmtTapName(%q) = %q (len %d), exceeds IFNAMSIZ", tt.instanceID, got, len(got))
			}
		})
	}
}

func TestOVSIfaceID(t *testing.T) {
	tests := []struct {
		eniID string
		want  string
	}{
		{"eni-abc123", "port-eni-abc123"},
		{"eni-", "port-eni-"},
		{"", "port-"},
	}
	for _, tt := range tests {
		got := OVSIfaceID(tt.eniID)
		if got != tt.want {
			t.Errorf("OVSIfaceID(%q) = %q, want %q", tt.eniID, got, tt.want)
		}
	}
}

func TestVPCTapSpec(t *testing.T) {
	got := VPCTapSpec("eni-abc123", "02:00:00:aa:bb:cc")
	want := TapSpec{
		Name:   "tapabc123",
		Bridge: "br-int",
		ExternalIDs: map[string]string{
			"iface-id":     "port-eni-abc123",
			"attached-mac": "02:00:00:aa:bb:cc",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("VPCTapSpec = %+v, want %+v", got, want)
	}
}

func TestSetupExtraENINICs_AppendsOnePerExtra(t *testing.T) {
	plumber := &fakeNetworkPlumber{}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	instance := &VM{
		ID: "i-multi",
		ExtraENIs: []ExtraENI{
			{ENIID: "eni-aaa", ENIMac: "02:00:00:aa:aa:aa", ENIIP: "10.0.1.4", SubnetID: "subnet-a"},
			{ENIID: "eni-bbb", ENIMac: "02:00:00:bb:bb:bb", ENIIP: "10.0.2.4", SubnetID: "subnet-b"},
		},
	}

	if err := m.setupExtraENINICs(instance); err != nil {
		t.Fatalf("setupExtraENINICs failed: %v", err)
	}

	if len(plumber.setupCalls) != 2 {
		t.Fatalf("expected 2 SetupTap calls, got %d", len(plumber.setupCalls))
	}
	want0 := VPCTapSpec("eni-aaa", "02:00:00:aa:aa:aa")
	if !reflect.DeepEqual(plumber.setupCalls[0], want0) {
		t.Errorf("first setup call = %+v, want %+v", plumber.setupCalls[0], want0)
	}
	want1 := VPCTapSpec("eni-bbb", "02:00:00:bb:bb:bb")
	if !reflect.DeepEqual(plumber.setupCalls[1], want1) {
		t.Errorf("second setup call = %+v, want %+v", plumber.setupCalls[1], want1)
	}

	if len(instance.Config.NetDevs) != 2 || len(instance.Config.Devices) != 2 {
		t.Fatalf("expected 2 netdevs + 2 devices, got %d + %d",
			len(instance.Config.NetDevs), len(instance.Config.Devices))
	}
	if !strings.Contains(instance.Config.NetDevs[0].Value, "id=net1") {
		t.Errorf("netdev[0] = %q, want id=net1", instance.Config.NetDevs[0].Value)
	}
	if !strings.Contains(instance.Config.NetDevs[1].Value, "id=net2") {
		t.Errorf("netdev[1] = %q, want id=net2", instance.Config.NetDevs[1].Value)
	}
	if !strings.Contains(instance.Config.Devices[0].Value, "mac=02:00:00:aa:aa:aa") {
		t.Errorf("device[0] = %q, missing primary MAC", instance.Config.Devices[0].Value)
	}
	if !strings.Contains(instance.Config.Devices[1].Value, "mac=02:00:00:bb:bb:bb") {
		t.Errorf("device[1] = %q, missing second MAC", instance.Config.Devices[1].Value)
	}
}

func TestSetupExtraENINICs_NoExtras_NoOp(t *testing.T) {
	plumber := &fakeNetworkPlumber{}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	instance := &VM{ID: "i-single"}

	if err := m.setupExtraENINICs(instance); err != nil {
		t.Fatalf("setupExtraENINICs failed: %v", err)
	}
	if len(plumber.setupCalls) != 0 {
		t.Errorf("expected zero setup calls for no extras, got %d", len(plumber.setupCalls))
	}
	if len(instance.Config.NetDevs) != 0 || len(instance.Config.Devices) != 0 {
		t.Errorf("expected no netdevs/devices, got %d/%d",
			len(instance.Config.NetDevs), len(instance.Config.Devices))
	}
}

func TestSetupExtraENINICs_TapSetupErrorReturns(t *testing.T) {
	plumber := &fakeNetworkPlumber{setupErr: errors.New("simulated tap failure")}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	instance := &VM{
		ID: "i-multi-err",
		ExtraENIs: []ExtraENI{
			{ENIID: "eni-aaa", ENIMac: "02:00:00:aa:aa:aa"},
			{ENIID: "eni-bbb", ENIMac: "02:00:00:bb:bb:bb"},
		},
	}

	err := m.setupExtraENINICs(instance)
	if err == nil {
		t.Fatal("expected error from failing tap setup, got nil")
	}
	if !strings.Contains(err.Error(), "eni-aaa") {
		t.Errorf("error = %v, want it to mention the failing ENI", err)
	}
	if len(plumber.setupCalls) != 1 {
		t.Errorf("expected 1 setup call before bailout, got %d", len(plumber.setupCalls))
	}
	if len(instance.Config.NetDevs) != 0 {
		t.Errorf("expected no netdevs on failure, got %d", len(instance.Config.NetDevs))
	}
}

func TestSetupExtraENINICs_NilPlumber_NoOp(t *testing.T) {
	m := NewManagerWithDeps(Deps{})
	instance := &VM{
		ID: "i-no-plumber",
		ExtraENIs: []ExtraENI{
			{ENIID: "eni-aaa", ENIMac: "02:00:00:aa:aa:aa"},
		},
	}
	if err := m.setupExtraENINICs(instance); err != nil {
		t.Fatalf("setupExtraENINICs without plumber should be a no-op, got %v", err)
	}
	if len(instance.Config.NetDevs) != 0 {
		t.Errorf("expected no netdevs without plumber, got %d", len(instance.Config.NetDevs))
	}
}

func TestCleanupExtraENITaps_CallsCleanupPerExtra(t *testing.T) {
	plumber := &fakeNetworkPlumber{}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	instance := &VM{
		ID: "i-multi-clean",
		ExtraENIs: []ExtraENI{
			{ENIID: "eni-111"},
			{ENIID: "eni-222"},
			{ENIID: "eni-333"},
		},
	}

	m.cleanupExtraENITaps(instance)

	if len(plumber.cleanupCalls) != 3 {
		t.Fatalf("expected 3 cleanup calls, got %d", len(plumber.cleanupCalls))
	}
	for i, eniID := range []string{"eni-111", "eni-222", "eni-333"} {
		want := TapDeviceName(eniID)
		if plumber.cleanupCalls[i] != want {
			t.Errorf("cleanup[%d] = %q, want %q", i, plumber.cleanupCalls[i], want)
		}
	}
}

func TestCleanupExtraENITaps_ErrorsAreLogged(t *testing.T) {
	plumber := &fakeNetworkPlumber{cleanupErr: errors.New("simulated cleanup failure")}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	instance := &VM{
		ID: "i-multi-clean-err",
		ExtraENIs: []ExtraENI{
			{ENIID: "eni-111"},
			{ENIID: "eni-222"},
		},
	}

	// Must not panic or return — errors are swallowed by design so partial
	// cleanup still frees later entries.
	m.cleanupExtraENITaps(instance)
	if len(plumber.cleanupCalls) != 2 {
		t.Errorf("expected both extras to be attempted, got %d cleanup calls", len(plumber.cleanupCalls))
	}
}

func TestCleanupExtraENITaps_NilPlumber_NoOp(t *testing.T) {
	m := NewManagerWithDeps(Deps{})
	instance := &VM{ID: "i-no-plumber", ExtraENIs: []ExtraENI{{ENIID: "eni-x"}}}
	// Must not panic when no plumber is wired.
	m.cleanupExtraENITaps(instance)
}
