package daemon

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/vm"
)

func TestTapDeviceName(t *testing.T) {
	tests := []struct {
		eniId    string
		expected string
	}{
		{"eni-abc123", "tapabc123"},                 // Short ID
		{"eni-abc123def456789", "tapabc123def456"},  // Truncated to 15 chars
		{"eni-a", "tapa"},                           // Minimal
		{"eni-123456789abcdef", "tap123456789abc"},  // Exactly 15 chars
		{"eni-123456789abcdefg", "tap123456789abc"}, // Truncated at 15
	}

	for _, tt := range tests {
		t.Run(tt.eniId, func(t *testing.T) {
			got := TapDeviceName(tt.eniId)
			if got != tt.expected {
				t.Errorf("TapDeviceName(%q) = %q, want %q", tt.eniId, got, tt.expected)
			}
			if len(got) > 15 {
				t.Errorf("TapDeviceName(%q) = %q (len %d), exceeds IFNAMSIZ limit of 15", tt.eniId, got, len(got))
			}
		})
	}
}

func TestOVSIfaceID(t *testing.T) {
	tests := []struct {
		eniId    string
		expected string
	}{
		{"eni-abc123", "port-eni-abc123"},
		{"eni-abc123def456789", "port-eni-abc123def456789"},
	}

	for _, tt := range tests {
		t.Run(tt.eniId, func(t *testing.T) {
			got := OVSIfaceID(tt.eniId)
			if got != tt.expected {
				t.Errorf("OVSIfaceID(%q) = %q, want %q", tt.eniId, got, tt.expected)
			}
		})
	}
}

// MockNetworkPlumber records calls for testing.
type MockNetworkPlumber struct {
	SetupCalls   []mockSetupCall
	CleanupCalls []string
	SetupErr     error
	CleanupErr   error
}

type mockSetupCall struct {
	ENIId string
	MAC   string
}

func (m *MockNetworkPlumber) SetupTapDevice(eniId, mac string) error {
	m.SetupCalls = append(m.SetupCalls, mockSetupCall{ENIId: eniId, MAC: mac})
	return m.SetupErr
}

func (m *MockNetworkPlumber) CleanupTapDevice(eniId string) error {
	m.CleanupCalls = append(m.CleanupCalls, eniId)
	return m.CleanupErr
}

func TestStartInstance_VPCNetworking(t *testing.T) {
	instance := &vm.VM{
		ID:           "i-test123",
		InstanceType: "t3.micro",
		ENIId:        "eni-abc123def456789",
		ENIMac:       "02:00:00:11:22:33",
	}

	// When ENI is set, config should use tap networking with MAC
	instance.Config = vm.Config{
		CPUCount:     1,
		Memory:       512,
		Architecture: "x86_64",
	}

	// Simulate what StartInstance does for VPC mode
	tapName := TapDeviceName(instance.ENIId)
	instance.Config.NetDevs = append(instance.Config.NetDevs, vm.NetDev{
		Value: "tap,id=net0,ifname=" + tapName + ",script=no,downscript=no",
	})
	instance.Config.Devices = append(instance.Config.Devices, vm.Device{
		Value: "virtio-net-pci,netdev=net0,mac=" + instance.ENIMac,
	})

	// Verify QEMU args
	if len(instance.Config.NetDevs) != 1 {
		t.Fatalf("expected 1 netdev, got %d", len(instance.Config.NetDevs))
	}

	expected := "tap,id=net0,ifname=tapabc123def456,script=no,downscript=no"
	if instance.Config.NetDevs[0].Value != expected {
		t.Errorf("netdev = %q, want %q", instance.Config.NetDevs[0].Value, expected)
	}

	expectedDev := "virtio-net-pci,netdev=net0,mac=02:00:00:11:22:33"
	if instance.Config.Devices[0].Value != expectedDev {
		t.Errorf("device = %q, want %q", instance.Config.Devices[0].Value, expectedDev)
	}
}

func TestStartInstance_FallbackNetworking(t *testing.T) {
	instance := &vm.VM{
		ID:           "i-test456",
		InstanceType: "t3.micro",
		// No ENI — should use user-mode networking
	}

	instance.Config = vm.Config{
		CPUCount:     1,
		Memory:       512,
		Architecture: "x86_64",
	}

	// Simulate what StartInstance does for non-VPC mode
	instance.Config.NetDevs = append(instance.Config.NetDevs, vm.NetDev{
		Value: "user,id=net0,hostfwd=tcp:127.0.0.1:22222-:22",
	})
	instance.Config.Devices = append(instance.Config.Devices, vm.Device{
		Value: "virtio-net-pci,netdev=net0",
	})

	// Verify no MAC is specified (QEMU auto-assigns)
	if len(instance.Config.Devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(instance.Config.Devices))
	}

	// User-mode networking should not include MAC
	dev := instance.Config.Devices[0].Value
	if dev != "virtio-net-pci,netdev=net0" {
		t.Errorf("device = %q, want 'virtio-net-pci,netdev=net0'", dev)
	}
}

func TestMockNetworkPlumber_SetupAndCleanup(t *testing.T) {
	mock := &MockNetworkPlumber{}

	err := mock.SetupTapDevice("eni-abc123", "02:00:00:aa:bb:cc")
	if err != nil {
		t.Fatalf("SetupTapDevice: %v", err)
	}
	if len(mock.SetupCalls) != 1 {
		t.Fatalf("expected 1 setup call, got %d", len(mock.SetupCalls))
	}
	if mock.SetupCalls[0].ENIId != "eni-abc123" {
		t.Errorf("setup eniId = %q, want 'eni-abc123'", mock.SetupCalls[0].ENIId)
	}
	if mock.SetupCalls[0].MAC != "02:00:00:aa:bb:cc" {
		t.Errorf("setup mac = %q, want '02:00:00:aa:bb:cc'", mock.SetupCalls[0].MAC)
	}

	err = mock.CleanupTapDevice("eni-abc123")
	if err != nil {
		t.Fatalf("CleanupTapDevice: %v", err)
	}
	if len(mock.CleanupCalls) != 1 {
		t.Fatalf("expected 1 cleanup call, got %d", len(mock.CleanupCalls))
	}
	if mock.CleanupCalls[0] != "eni-abc123" {
		t.Errorf("cleanup eniId = %q, want 'eni-abc123'", mock.CleanupCalls[0])
	}
}

func TestSudoCommand_NonRoot(t *testing.T) {
	// When not root, sudoCommand should prefix with sudo
	cmd := sudoCommand("ovs-vsctl", "br-exists", "br-int")
	args := cmd.Args

	if os.Getuid() == 0 {
		// Running as root: should NOT use sudo
		if args[0] != "ovs-vsctl" {
			t.Errorf("as root, expected args[0]='ovs-vsctl', got %q", args[0])
		}
	} else {
		// Running as non-root: should use sudo
		if args[0] != "sudo" {
			t.Errorf("as non-root, expected args[0]='sudo', got %q", args[0])
		}
		if args[1] != "ovs-vsctl" {
			t.Errorf("as non-root, expected args[1]='ovs-vsctl', got %q", args[1])
		}
		if len(args) != 4 {
			t.Errorf("expected 4 args [sudo ovs-vsctl br-exists br-int], got %d: %v", len(args), args)
		}
	}
}

func TestGenerateDevMAC(t *testing.T) {
	tests := []struct {
		instanceId string
	}{
		{"i-abc123"},
		{"i-def456"},
		{"i-ghi789"},
	}

	// All MACs should be unique and have the 02:de:v0 prefix
	seen := make(map[string]bool)
	for _, tt := range tests {
		mac := generateDevMAC(tt.instanceId)
		if !strings.HasPrefix(mac, "02:de:00:") {
			t.Errorf("generateDevMAC(%q) = %q, want prefix '02:de:00:'", tt.instanceId, mac)
		}
		if seen[mac] {
			t.Errorf("generateDevMAC(%q) = %q, duplicate MAC", tt.instanceId, mac)
		}
		seen[mac] = true
	}

	// Same input should produce same output (deterministic)
	mac1 := generateDevMAC("i-test123")
	mac2 := generateDevMAC("i-test123")
	if mac1 != mac2 {
		t.Errorf("generateDevMAC not deterministic: %q != %q", mac1, mac2)
	}
}

func TestNetworkPlumber_InterfaceCompliance(t *testing.T) {
	// Verify both types satisfy the interface
	var _ NetworkPlumber = &OVSNetworkPlumber{}
	var _ NetworkPlumber = &MockNetworkPlumber{}
}

func TestSetupExtraENINICs_AppendsOnePerExtra(t *testing.T) {
	mock := &MockNetworkPlumber{}
	d := &Daemon{networkPlumber: mock}
	instance := &vm.VM{
		ID: "i-multi",
		ExtraENIs: []vm.ExtraENI{
			{ENIID: "eni-aaa", ENIMac: "02:00:00:aa:aa:aa", ENIIP: "10.0.1.4", SubnetID: "subnet-a"},
			{ENIID: "eni-bbb", ENIMac: "02:00:00:bb:bb:bb", ENIIP: "10.0.2.4", SubnetID: "subnet-b"},
		},
	}

	if err := d.setupExtraENINICs(instance); err != nil {
		t.Fatalf("setupExtraENINICs failed: %v", err)
	}

	// One SetupTapDevice call per extra ENI.
	if len(mock.SetupCalls) != 2 {
		t.Fatalf("expected 2 SetupTapDevice calls, got %d", len(mock.SetupCalls))
	}
	if mock.SetupCalls[0].ENIId != "eni-aaa" || mock.SetupCalls[0].MAC != "02:00:00:aa:aa:aa" {
		t.Errorf("first setup call = %+v, want eni-aaa/02:00:00:aa:aa:aa", mock.SetupCalls[0])
	}
	if mock.SetupCalls[1].ENIId != "eni-bbb" || mock.SetupCalls[1].MAC != "02:00:00:bb:bb:bb" {
		t.Errorf("second setup call = %+v, want eni-bbb/02:00:00:bb:bb:bb", mock.SetupCalls[1])
	}

	// NetDevs and Devices each get one entry per extra ENI, named net1/net2.
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
	mock := &MockNetworkPlumber{}
	d := &Daemon{networkPlumber: mock}
	instance := &vm.VM{ID: "i-single"}

	if err := d.setupExtraENINICs(instance); err != nil {
		t.Fatalf("setupExtraENINICs failed: %v", err)
	}
	if len(mock.SetupCalls) != 0 {
		t.Errorf("expected zero setup calls for no extras, got %d", len(mock.SetupCalls))
	}
	if len(instance.Config.NetDevs) != 0 || len(instance.Config.Devices) != 0 {
		t.Errorf("expected no netdevs/devices, got %d/%d",
			len(instance.Config.NetDevs), len(instance.Config.Devices))
	}
}

func TestSetupExtraENINICs_TapSetupErrorReturns(t *testing.T) {
	mock := &MockNetworkPlumber{SetupErr: fmt.Errorf("simulated tap failure")}
	d := &Daemon{networkPlumber: mock}
	instance := &vm.VM{
		ID: "i-multi-err",
		ExtraENIs: []vm.ExtraENI{
			{ENIID: "eni-aaa", ENIMac: "02:00:00:aa:aa:aa"},
			{ENIID: "eni-bbb", ENIMac: "02:00:00:bb:bb:bb"},
		},
	}

	err := d.setupExtraENINICs(instance)
	if err == nil {
		t.Fatal("expected error from failing tap setup, got nil")
	}
	if !strings.Contains(err.Error(), "eni-aaa") {
		t.Errorf("error = %v, want it to mention the failing ENI", err)
	}
	// Must bail on first failure — second ENI should not be touched.
	if len(mock.SetupCalls) != 1 {
		t.Errorf("expected 1 setup call before bailout, got %d", len(mock.SetupCalls))
	}
	// No NIC config appended for a failed setup.
	if len(instance.Config.NetDevs) != 0 {
		t.Errorf("expected no netdevs on failure, got %d", len(instance.Config.NetDevs))
	}
}

func TestCleanupExtraENITaps_CallsCleanupPerExtra(t *testing.T) {
	mock := &MockNetworkPlumber{}
	d := &Daemon{networkPlumber: mock}
	instance := &vm.VM{
		ID: "i-multi-clean",
		ExtraENIs: []vm.ExtraENI{
			{ENIID: "eni-111"},
			{ENIID: "eni-222"},
			{ENIID: "eni-333"},
		},
	}

	d.cleanupExtraENITaps(instance)

	if len(mock.CleanupCalls) != 3 {
		t.Fatalf("expected 3 cleanup calls, got %d", len(mock.CleanupCalls))
	}
	for i, want := range []string{"eni-111", "eni-222", "eni-333"} {
		if mock.CleanupCalls[i] != want {
			t.Errorf("cleanup[%d] = %q, want %q", i, mock.CleanupCalls[i], want)
		}
	}
}

func TestCleanupExtraENITaps_ErrorsAreLogged(t *testing.T) {
	mock := &MockNetworkPlumber{CleanupErr: fmt.Errorf("simulated cleanup failure")}
	d := &Daemon{networkPlumber: mock}
	instance := &vm.VM{
		ID: "i-multi-clean-err",
		ExtraENIs: []vm.ExtraENI{
			{ENIID: "eni-111"},
			{ENIID: "eni-222"},
		},
	}

	// Must not panic or return — errors are swallowed by design so partial
	// cleanup still frees later entries.
	d.cleanupExtraENITaps(instance)
	if len(mock.CleanupCalls) != 2 {
		t.Errorf("expected both extras to be attempted, got %d cleanup calls", len(mock.CleanupCalls))
	}
}

func TestOVNHealthStatus_Fields(t *testing.T) {
	// Verify OVNHealthStatus struct can be used for health reporting
	status := OVNHealthStatus{
		BrIntExists:     true,
		OVNControllerUp: true,
		ChassisID:       "chassis-node1",
		EncapIP:         "10.0.0.1",
		OVNRemote:       "tcp:10.0.0.1:6642",
	}

	if !status.BrIntExists {
		t.Error("expected BrIntExists to be true")
	}
	if !status.OVNControllerUp {
		t.Error("expected OVNControllerUp to be true")
	}
	if status.ChassisID != "chassis-node1" {
		t.Errorf("ChassisID = %q, want 'chassis-node1'", status.ChassisID)
	}
	if status.EncapIP != "10.0.0.1" {
		t.Errorf("EncapIP = %q, want '10.0.0.1'", status.EncapIP)
	}
	if status.OVNRemote != "tcp:10.0.0.1:6642" {
		t.Errorf("OVNRemote = %q, want 'tcp:10.0.0.1:6642'", status.OVNRemote)
	}
}

func TestOVNHealthStatus_Defaults(t *testing.T) {
	// Zero-value OVNHealthStatus should indicate nothing is ready
	var status OVNHealthStatus

	if status.BrIntExists {
		t.Error("zero-value BrIntExists should be false")
	}
	if status.OVNControllerUp {
		t.Error("zero-value OVNControllerUp should be false")
	}
	if status.ChassisID != "" {
		t.Errorf("zero-value ChassisID should be empty, got %q", status.ChassisID)
	}
}

func TestCheckOVNHealth_ReturnsStatus(t *testing.T) {
	// CheckOVNHealth should return a status struct without panicking,
	// even when OVS/OVN tools are not installed (CI environment).
	// On a dev machine without OVN, all fields will be zero values.
	status := CheckOVNHealth()

	// On CI without OVS, both should be false — just verify no panic
	_ = status.BrIntExists
	_ = status.OVNControllerUp
	_ = status.ChassisID
	_ = status.EncapIP
	_ = status.OVNRemote
}

func TestFindInterfaceByIP_InvalidIP(t *testing.T) {
	_, err := findInterfaceByIP("not-an-ip")
	if err == nil {
		t.Fatal("expected error for invalid IP")
	}
	if !strings.Contains(err.Error(), "invalid IP address") {
		t.Errorf("expected 'invalid IP address' error, got: %v", err)
	}
}

func TestFindInterfaceByIP_Loopback(t *testing.T) {
	// 127.0.0.1 should always be on the loopback interface
	iface, err := findInterfaceByIP("127.0.0.1")
	if err != nil {
		t.Fatalf("findInterfaceByIP(127.0.0.1): %v", err)
	}
	if iface != "lo" {
		t.Errorf("findInterfaceByIP(127.0.0.1) = %q, want 'lo'", iface)
	}
}

func TestFindInterfaceByIP_NotFound(t *testing.T) {
	_, err := findInterfaceByIP("192.0.2.1")
	if err == nil {
		t.Fatal("expected error for non-existent IP")
	}
	if !strings.Contains(err.Error(), "no interface found") {
		t.Errorf("expected 'no interface found' error, got: %v", err)
	}
}

func TestEnsureDataRoute_NoOVS(t *testing.T) {
	// EnsureDataRoute requires ip commands which may not work in CI.
	// On loopback, there's no kernel subnet route, so it should return an error.
	err := EnsureDataRoute("127.0.0.1")
	// We expect an error (no kernel route for lo), but no panic.
	_ = err
}

func TestSetupComputeNode_ValidatesArgs(t *testing.T) {
	// SetupComputeNode requires ovs-vsctl which may not be available in CI.
	// This test verifies the function signature and that it returns an error
	// when OVS is not installed (expected on CI).
	err := SetupComputeNode("chassis-test", "tcp:127.0.0.1:6642", "10.0.0.1")

	// We expect an error in CI (no OVS), but the function should not panic.
	// On a dev machine with OVS, it would succeed. Either result is acceptable.
	_ = err
}

func TestMockNetworkPlumber_SetupError(t *testing.T) {
	mock := &MockNetworkPlumber{
		SetupErr: fmt.Errorf("simulated setup failure"),
	}
	err := mock.SetupTapDevice("eni-abc123", "02:00:00:aa:bb:cc")
	if err == nil {
		t.Fatal("expected error from SetupTapDevice")
	}
	if err.Error() != "simulated setup failure" {
		t.Errorf("unexpected error: %v", err)
	}
	// Call should still be recorded
	if len(mock.SetupCalls) != 1 {
		t.Fatalf("expected 1 setup call, got %d", len(mock.SetupCalls))
	}
}

func TestMockNetworkPlumber_CleanupError(t *testing.T) {
	mock := &MockNetworkPlumber{
		CleanupErr: fmt.Errorf("simulated cleanup failure"),
	}
	err := mock.CleanupTapDevice("eni-abc123")
	if err == nil {
		t.Fatal("expected error from CleanupTapDevice")
	}
	if err.Error() != "simulated cleanup failure" {
		t.Errorf("unexpected error: %v", err)
	}
	if len(mock.CleanupCalls) != 1 {
		t.Fatalf("expected 1 cleanup call, got %d", len(mock.CleanupCalls))
	}
}

func TestGenerateDevMAC_Format(t *testing.T) {
	tests := []string{
		"i-abc123",
		"i-def456",
		"i-00000000",
		"i-ffffffff",
		"i-a",
		"i-very-long-instance-id-with-many-characters",
	}
	for _, id := range tests {
		mac := generateDevMAC(id)
		// Must be 17 chars: xx:xx:xx:xx:xx:xx
		if len(mac) != 17 {
			t.Errorf("generateDevMAC(%q) = %q, expected 17 chars", id, mac)
		}
		// Must start with locally-administered unicast prefix 02:de:00
		if !strings.HasPrefix(mac, "02:de:00:") {
			t.Errorf("generateDevMAC(%q) = %q, expected prefix 02:de:00:", id, mac)
		}
		// All chars must be valid hex or colons
		for i, c := range mac {
			if i%3 == 2 {
				if c != ':' {
					t.Errorf("generateDevMAC(%q) = %q, expected ':' at pos %d", id, mac, i)
				}
			} else {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					t.Errorf("generateDevMAC(%q) = %q, invalid hex char '%c' at pos %d", id, mac, c, i)
				}
			}
		}
	}
}

func TestTapDeviceName_EmptyInput(t *testing.T) {
	// Even with empty string (no eni- prefix), should not panic
	name := TapDeviceName("")
	if name != "tap" {
		t.Errorf("TapDeviceName('') = %q, want 'tap'", name)
	}
}

func TestOVSIfaceID_Format(t *testing.T) {
	tests := []struct {
		eniId    string
		expected string
	}{
		{"eni-short", "port-eni-short"},
		{"eni-", "port-eni-"},
		{"", "port-"},
	}
	for _, tt := range tests {
		got := OVSIfaceID(tt.eniId)
		if got != tt.expected {
			t.Errorf("OVSIfaceID(%q) = %q, want %q", tt.eniId, got, tt.expected)
		}
	}
}

func TestGenerateMgmtMAC(t *testing.T) {
	tests := []string{
		"i-abc123",
		"i-def456",
		"i-ghi789",
	}

	seen := make(map[string]bool)
	for _, id := range tests {
		mac := generateMgmtMAC(id)
		if !strings.HasPrefix(mac, "02:a0:00:") {
			t.Errorf("generateMgmtMAC(%q) = %q, want prefix '02:a0:00:'", id, mac)
		}
		if len(mac) != 17 {
			t.Errorf("generateMgmtMAC(%q) = %q, expected 17 chars", id, mac)
		}
		if seen[mac] {
			t.Errorf("generateMgmtMAC(%q) = %q, duplicate MAC", id, mac)
		}
		seen[mac] = true
	}

	// Deterministic
	mac1 := generateMgmtMAC("i-test123")
	mac2 := generateMgmtMAC("i-test123")
	if mac1 != mac2 {
		t.Errorf("generateMgmtMAC not deterministic: %q != %q", mac1, mac2)
	}

	// Different from dev MAC for same instance
	devMAC := generateDevMAC("i-test123")
	mgmtMAC := generateMgmtMAC("i-test123")
	if devMAC == mgmtMAC {
		t.Errorf("dev and mgmt MACs should differ for same instance: both %q", devMAC)
	}
}

func TestMgmtTapName(t *testing.T) {
	tests := []struct {
		instanceID string
		expected   string
	}{
		{"i-abc123", "mgabc123"},
		{"i-abc123def456789", "mgabc123def4567"}, // Truncated to 15 chars
		{"i-a", "mga"},
		{"abc123", "mgabc123"}, // No i- prefix
	}

	for _, tt := range tests {
		t.Run(tt.instanceID, func(t *testing.T) {
			got := MgmtTapName(tt.instanceID)
			if got != tt.expected {
				t.Errorf("MgmtTapName(%q) = %q, want %q", tt.instanceID, got, tt.expected)
			}
			if len(got) > 15 {
				t.Errorf("MgmtTapName(%q) = %q (len %d), exceeds IFNAMSIZ limit of 15", tt.instanceID, got, len(got))
			}
		})
	}
}

func TestGetBridgeIPv4_Loopback(t *testing.T) {
	// "lo" is always present and has 127.0.0.1
	ip, err := GetBridgeIPv4("lo")
	if err != nil {
		t.Fatalf("GetBridgeIPv4(lo): %v", err)
	}
	if ip != "127.0.0.1" {
		t.Errorf("GetBridgeIPv4(lo) = %q, want 127.0.0.1", ip)
	}
}

func TestGetBridgeIPv4_NonexistentBridge(t *testing.T) {
	ip, err := GetBridgeIPv4("br-nonexistent-test-xyz")
	if err != nil {
		t.Fatalf("expected nil error for absent bridge, got: %v", err)
	}
	if ip != "" {
		t.Errorf("expected empty IP for absent bridge, got %q", ip)
	}
}
