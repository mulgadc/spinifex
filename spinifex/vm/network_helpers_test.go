package vm

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// fakeNetworkPlumber records calls so tests can assert per-spec behaviour.
type fakeNetworkPlumber struct {
	setupCalls        []TapSpec
	cleanupCalls      []string
	imdsAttachCalls   []imdsAttachCall
	imdsDetachCalls   []string
	ensureBridgeCalls int
	setupErr          error
	cleanupErr        error
	imdsAttachErr     error
	imdsDetachErr     error
	ensureBridgeErr   error
}

// imdsAttachCall captures the args of one AttachIMDSDatapath invocation.
type imdsAttachCall struct {
	eniID    string
	mac      string
	subnetID string
}

func (p *fakeNetworkPlumber) SetupTap(spec TapSpec) error {
	p.setupCalls = append(p.setupCalls, spec)
	return p.setupErr
}

func (p *fakeNetworkPlumber) CleanupTap(name string) error {
	p.cleanupCalls = append(p.cleanupCalls, name)
	return p.cleanupErr
}

func (p *fakeNetworkPlumber) AttachIMDSDatapath(eniID, mac, subnetID string) error {
	p.imdsAttachCalls = append(p.imdsAttachCalls, imdsAttachCall{eniID: eniID, mac: mac, subnetID: subnetID})
	return p.imdsAttachErr
}

func (p *fakeNetworkPlumber) DetachIMDSDatapath(eniID string) error {
	p.imdsDetachCalls = append(p.imdsDetachCalls, eniID)
	return p.imdsDetachErr
}

func (p *fakeNetworkPlumber) EnsureIMDSDatapathBridge() error {
	p.ensureBridgeCalls++
	return p.ensureBridgeErr
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

func TestIMDSPrimaryTapSpec(t *testing.T) {
	got := IMDSPrimaryTapSpec("eni-abc123")
	want := TapSpec{
		Name:   "tapabc123",
		Bridge: IMDSBridgeName,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("IMDSPrimaryTapSpec = %+v, want %+v", got, want)
	}
	// The primary tap carries no OVN binding — the patch's br-int end does.
	if got.ExternalIDs != nil {
		t.Errorf("IMDSPrimaryTapSpec must carry no external_ids, got %v", got.ExternalIDs)
	}
	if got.Bridge == "br-int" {
		t.Error("IMDSPrimaryTapSpec must not place the primary tap on br-int")
	}
}

func TestAttachPrimaryIMDSDatapath(t *testing.T) {
	primary := &VM{
		ID:       "i-primary",
		ENIId:    "eni-abc123",
		ENIMac:   "02:00:00:aa:bb:cc",
		Instance: &ec2.Instance{SubnetId: aws.String("subnet-xyz")},
	}

	tests := []struct {
		name     string
		instance *VM
		plumber  *fakeNetworkPlumber
		want     []imdsAttachCall
	}{
		{
			name:     "primary ENI with subnet attaches once",
			instance: primary,
			plumber:  &fakeNetworkPlumber{},
			want:     []imdsAttachCall{{eniID: "eni-abc123", mac: "02:00:00:aa:bb:cc", subnetID: "subnet-xyz"}},
		},
		{
			name:     "no primary ENI is a no-op",
			instance: &VM{ID: "i-no-eni", Instance: &ec2.Instance{SubnetId: aws.String("subnet-xyz")}},
			plumber:  &fakeNetworkPlumber{},
			want:     nil,
		},
		{
			name:     "missing subnet is a no-op",
			instance: &VM{ID: "i-no-subnet", ENIId: "eni-abc123", ENIMac: "02:00:00:aa:bb:cc"},
			plumber:  &fakeNetworkPlumber{},
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewManagerWithDeps(Deps{NetworkPlumber: tt.plumber})
			if err := m.attachPrimaryIMDSDatapath(tt.instance); err != nil {
				t.Fatalf("attachPrimaryIMDSDatapath returned %v, want nil", err)
			}
			if !reflect.DeepEqual(tt.plumber.imdsAttachCalls, tt.want) {
				t.Errorf("imdsAttachCalls = %+v, want %+v", tt.plumber.imdsAttachCalls, tt.want)
			}
			// A clean attach (or a no-op) must never roll the tap back to br-int.
			if len(tt.plumber.imdsDetachCalls) != 0 || len(tt.plumber.cleanupCalls) != 0 || len(tt.plumber.setupCalls) != 0 {
				t.Errorf("unexpected rollback: detach=%d cleanup=%d setup=%d",
					len(tt.plumber.imdsDetachCalls), len(tt.plumber.cleanupCalls), len(tt.plumber.setupCalls))
			}
		})
	}
}

func TestAttachPrimaryIMDSDatapath_NilPlumber_NoOp(t *testing.T) {
	m := NewManagerWithDeps(Deps{})
	// Must not panic without a plumber.
	if err := m.attachPrimaryIMDSDatapath(&VM{
		ID:       "i-no-plumber",
		ENIId:    "eni-abc123",
		Instance: &ec2.Instance{SubnetId: aws.String("subnet-xyz")},
	}); err != nil {
		t.Fatalf("attachPrimaryIMDSDatapath without plumber should be a no-op, got %v", err)
	}
}

func TestAttachPrimaryIMDSDatapath_ServingDegradedIsFatal(t *testing.T) {
	// Serving-only failure: connectivity (patch + forward flows) is intact, only the
	// IMDS demux/reply stage failed. Guest bootstrap now comes from IMDS, so an
	// unreachable IMDS would boot a silently-unconfigured guest — the launch must
	// fail. No roll back to br-int (a rolled-back tap is equally unreachable for IMDS).
	plumber := &fakeNetworkPlumber{
		imdsAttachErr: fmt.Errorf("%w: install endpoint: ovsdb busy", ErrIMDSServingDegraded),
	}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	err := m.attachPrimaryIMDSDatapath(&VM{
		ID:       "i-serving-degraded",
		ENIId:    "eni-abc123",
		ENIMac:   "02:00:00:aa:bb:cc",
		Instance: &ec2.Instance{SubnetId: aws.String("subnet-xyz")},
	})
	if err == nil {
		t.Fatal("serving-degraded attach must fail the launch, got nil")
	}
	if !errors.Is(err, ErrIMDSServingDegraded) {
		t.Errorf("error = %v, want it to wrap ErrIMDSServingDegraded", err)
	}
	if len(plumber.imdsAttachCalls) != 1 {
		t.Errorf("expected the attach to be attempted once, got %d calls", len(plumber.imdsAttachCalls))
	}
	if len(plumber.imdsDetachCalls) != 0 || len(plumber.cleanupCalls) != 0 || len(plumber.setupCalls) != 0 {
		t.Errorf("must not roll back: detach=%d cleanup=%d setup=%d",
			len(plumber.imdsDetachCalls), len(plumber.cleanupCalls), len(plumber.setupCalls))
	}
}

func TestAttachPrimaryIMDSDatapath_ConnectivityFailureIsFatal(t *testing.T) {
	// Connectivity-critical failure: the primary tap is stranded on br-imds. The
	// launch must fail — there is no roll back to br-int, since a rolled-back tap is
	// equally unreachable for IMDS and a clear error beats a half-working box.
	plumber := &fakeNetworkPlumber{imdsAttachErr: errors.New("ovsdb lock held")}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	err := m.attachPrimaryIMDSDatapath(&VM{
		ID:       "i-conn-err",
		ENIId:    "eni-abc123",
		ENIMac:   "02:00:00:aa:bb:cc",
		Instance: &ec2.Instance{SubnetId: aws.String("subnet-xyz")},
	})
	if err == nil {
		t.Fatal("connectivity-critical attach must fail the launch, got nil")
	}
	if !strings.Contains(err.Error(), "ovsdb lock held") {
		t.Errorf("error = %v, want it to wrap the attach failure", err)
	}
	// No rollback: the tap is not detached, cleaned up, or re-plumbed onto br-int.
	if len(plumber.imdsDetachCalls) != 0 || len(plumber.cleanupCalls) != 0 || len(plumber.setupCalls) != 0 {
		t.Errorf("must not roll back: detach=%d cleanup=%d setup=%d",
			len(plumber.imdsDetachCalls), len(plumber.cleanupCalls), len(plumber.setupCalls))
	}
}

func TestDetachPrimaryIMDSDatapath(t *testing.T) {
	tests := []struct {
		name     string
		instance *VM
		want     []string
	}{
		{
			name:     "primary ENI detaches once by ENI ID",
			instance: &VM{ID: "i-primary", ENIId: "eni-abc123"},
			want:     []string{"eni-abc123"},
		},
		{
			name:     "no primary ENI is a no-op",
			instance: &VM{ID: "i-no-eni"},
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plumber := &fakeNetworkPlumber{}
			m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
			m.detachPrimaryIMDSDatapath(tt.instance)
			if !reflect.DeepEqual(plumber.imdsDetachCalls, tt.want) {
				t.Errorf("imdsDetachCalls = %+v, want %+v", plumber.imdsDetachCalls, tt.want)
			}
		})
	}
}

func TestDetachPrimaryIMDSDatapath_NilPlumber_NoOp(t *testing.T) {
	m := NewManagerWithDeps(Deps{})
	// Must not panic without a plumber.
	m.detachPrimaryIMDSDatapath(&VM{ID: "i-no-plumber", ENIId: "eni-abc123"})
}

func TestDetachPrimaryIMDSDatapath_DetachErrorIsNonFatal(t *testing.T) {
	plumber := &fakeNetworkPlumber{imdsDetachErr: errors.New("simulated detach failure")}
	m := NewManagerWithDeps(Deps{NetworkPlumber: plumber})
	// Best-effort: a failing detach must not panic or surface — teardown
	// continues so the tap and remaining state are still cleaned up.
	m.detachPrimaryIMDSDatapath(&VM{ID: "i-detach-err", ENIId: "eni-abc123"})
	if len(plumber.imdsDetachCalls) != 1 {
		t.Errorf("expected the detach to be attempted once, got %d calls", len(plumber.imdsDetachCalls))
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
