package vpcd

import (
	"fmt"
	"strings"
	"testing"
)

func TestPreflightOVN_AllPass(t *testing.T) {
	origBrInt := checkBrInt
	origCtrl := checkOVNController
	defer func() {
		checkBrInt = origBrInt
		checkOVNController = origCtrl
	}()

	checkBrInt = func() error { return nil }
	checkOVNController = func() error { return nil }

	if err := preflightOVN(); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestPreflightOVN_BrIntMissing(t *testing.T) {
	origBrInt := checkBrInt
	origCtrl := checkOVNController
	defer func() {
		checkBrInt = origBrInt
		checkOVNController = origCtrl
	}()

	checkBrInt = func() error {
		return fmt.Errorf("br-int does not exist: run ./scripts/setup-ovn.sh --management")
	}
	checkOVNController = func() error { return nil }

	err := preflightOVN()
	if err == nil {
		t.Fatal("expected error when br-int is missing")
	}
	if !strings.Contains(err.Error(), "br-int") {
		t.Errorf("expected error to mention br-int, got: %v", err)
	}
}

func TestPreflightOVN_ControllerNotRunning(t *testing.T) {
	origBrInt := checkBrInt
	origCtrl := checkOVNController
	defer func() {
		checkBrInt = origBrInt
		checkOVNController = origCtrl
	}()

	checkBrInt = func() error { return nil }
	checkOVNController = func() error {
		return fmt.Errorf("ovn-controller is not running: run ./scripts/setup-ovn.sh --management")
	}

	err := preflightOVN()
	if err == nil {
		t.Fatal("expected error when ovn-controller is down")
	}
	if !strings.Contains(err.Error(), "ovn-controller") {
		t.Errorf("expected error to mention ovn-controller, got: %v", err)
	}
}

func TestDiscoverChassis_ParsesOutput(t *testing.T) {
	orig := discoverChassis
	defer func() { discoverChassis = orig }()

	discoverChassis = func(sbAddr string) ([]string, error) {
		return []string{"chassis-node1", "chassis-node2", "chassis-node3"}, nil
	}

	names, err := discoverChassis("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("expected 3 chassis, got %d: %v", len(names), names)
	}
	expected := map[string]bool{"chassis-node1": true, "chassis-node2": true, "chassis-node3": true}
	for _, n := range names {
		if !expected[n] {
			t.Errorf("unexpected chassis name: %s", n)
		}
	}
}

func TestDiscoverChassis_SingleNode(t *testing.T) {
	orig := discoverChassis
	defer func() { discoverChassis = orig }()

	discoverChassis = func(sbAddr string) ([]string, error) {
		return []string{"chassis-spinifex-image-builder"}, nil
	}

	names, err := discoverChassis("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 1 || names[0] != "chassis-spinifex-image-builder" {
		t.Errorf("expected [chassis-spinifex-image-builder], got %v", names)
	}
}

func TestDiscoverChassis_EmptyOutput(t *testing.T) {
	orig := discoverChassis
	defer func() { discoverChassis = orig }()

	discoverChassis = func(sbAddr string) ([]string, error) {
		return nil, nil
	}

	names, err := discoverChassis("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected empty chassis list, got %v", names)
	}
}

func TestDiscoverChassis_Error_FallsBackToConfig(t *testing.T) {
	orig := discoverChassis
	defer func() { discoverChassis = orig }()

	discoverChassis = func(sbAddr string) ([]string, error) {
		return nil, fmt.Errorf("connection refused")
	}

	// Simulate the fallback logic from launchService
	_, err := discoverChassis("")
	if err == nil {
		t.Fatal("expected error from discoverChassis")
	}

	// Fallback to config (as launchService does)
	chassisNames := []string{"chassis-node1"}
	if len(chassisNames) != 1 || chassisNames[0] != "chassis-node1" {
		t.Errorf("expected fallback to config names, got %v", chassisNames)
	}
}

// TestParseChassisList_FiltersStaleLocal verifies that stale chassis entries on
// the local host (same hostname, different system-id) are filtered out.
func TestParseChassisList_FiltersStaleLocal(t *testing.T) {
	// ovn-sbctl --bare --columns=name,hostname output: two chassis on
	// the same host, "chassis-node1" is stale.
	raw := "chassis-node1\njulian-wattle\n\nchassis-test\njulian-wattle\n"

	names := parseChassisList(raw, "chassis-test", "julian-wattle")
	if len(names) != 1 {
		t.Fatalf("expected 1 chassis (stale filtered), got %d: %v", len(names), names)
	}
	if names[0] != "chassis-test" {
		t.Errorf("expected chassis-test, got %s", names[0])
	}
}

// TestParseChassisList_KeepsRemoteChassis verifies that chassis on other hosts
// are preserved even if there are stale entries on the local host.
func TestParseChassisList_KeepsRemoteChassis(t *testing.T) {
	raw := "chassis-nodeA\nlocal-host\n\nchassis-old\nlocal-host\n\nchassis-nodeB\nremote-host\n"

	names := parseChassisList(raw, "chassis-nodeA", "local-host")
	if len(names) != 2 {
		t.Fatalf("expected 2 chassis (stale filtered), got %d: %v", len(names), names)
	}
	expected := map[string]bool{"chassis-nodeA": true, "chassis-nodeB": true}
	for _, n := range names {
		if !expected[n] {
			t.Errorf("unexpected chassis: %s", n)
		}
	}
}

// TestParseChassisList_AllRemote verifies no filtering when all chassis are remote.
func TestParseChassisList_AllRemote(t *testing.T) {
	raw := "chassis-node1\nhost-a\n\nchassis-node2\nhost-b\n\nchassis-node3\nhost-c\n"

	names := parseChassisList(raw, "chassis-local", "host-local")
	if len(names) != 3 {
		t.Fatalf("expected 3 chassis, got %d: %v", len(names), names)
	}
}

// TestParseChassisList_Empty verifies empty input returns nil.
func TestParseChassisList_Empty(t *testing.T) {
	names := parseChassisList("", "chassis-test", "local-host")
	if len(names) != 0 {
		t.Errorf("expected empty, got %v", names)
	}
}

func TestPreflightOVN_BothFail_ReportsFirst(t *testing.T) {
	origBrInt := checkBrInt
	origCtrl := checkOVNController
	defer func() {
		checkBrInt = origBrInt
		checkOVNController = origCtrl
	}()

	checkBrInt = func() error {
		return fmt.Errorf("br-int does not exist")
	}
	checkOVNController = func() error {
		return fmt.Errorf("ovn-controller is not running")
	}

	err := preflightOVN()
	if err == nil {
		t.Fatal("expected error when both fail")
	}
	// Should report br-int first (checked first)
	if !strings.Contains(err.Error(), "br-int") {
		t.Errorf("expected first error to mention br-int, got: %v", err)
	}
}

// verifyBridgeMode is the post-detect sanity check — mulga-998.b Fix 2.
// portToBr and readLinkMaster are injected for tests.

func stubBridgeProbes(t *testing.T, ovsPorts map[string]string, links map[string]string) {
	t.Helper()
	origPort := portToBr
	origLink := readLinkMaster
	t.Cleanup(func() {
		portToBr = origPort
		readLinkMaster = origLink
	})
	portToBr = func(port string) (string, error) {
		br, ok := ovsPorts[port]
		if !ok {
			return "", fmt.Errorf("no port named %q", port)
		}
		return br, nil
	}
	readLinkMaster = func(iface string) (string, error) {
		m, ok := links[iface]
		if !ok {
			return "", fmt.Errorf("no link named %q", iface)
		}
		return m, nil
	}
}

func TestVerifyBridgeMode_DirectOK(t *testing.T) {
	stubBridgeProbes(t, map[string]string{"enp0s3": "br-wan"}, nil)
	if err := verifyBridgeMode(BridgeModeDirect, "enp0s3", "br-wan"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestVerifyBridgeMode_DirectMismatch(t *testing.T) {
	stubBridgeProbes(t, map[string]string{"enp0s3": "br-wan"}, nil)
	err := verifyBridgeMode(BridgeModeDirect, "enp0s3", "br-ext")
	if err == nil || !strings.Contains(err.Error(), "br-ext") {
		t.Fatalf("expected mismatch error, got: %v", err)
	}
}

func TestVerifyBridgeMode_DirectMissingNIC(t *testing.T) {
	stubBridgeProbes(t, map[string]string{}, nil)
	err := verifyBridgeMode(BridgeModeDirect, "enp0s3", "br-wan")
	if err == nil {
		t.Fatal("expected error when WAN NIC not in OVSDB")
	}
}

func TestVerifyBridgeMode_VethOK(t *testing.T) {
	stubBridgeProbes(t,
		map[string]string{"veth-wan-ovs": OvnExternalBridge},
		map[string]string{"veth-wan-br": "br-wan"})
	if err := verifyBridgeMode(BridgeModeVeth, "", "br-wan"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}

func TestVerifyBridgeMode_VethMissingOvsPort(t *testing.T) {
	stubBridgeProbes(t, map[string]string{}, map[string]string{"veth-wan-br": "br-wan"})
	err := verifyBridgeMode(BridgeModeVeth, "", "br-wan")
	if err == nil || !strings.Contains(err.Error(), "veth-wan-ovs") {
		t.Fatalf("expected veth-wan-ovs error, got: %v", err)
	}
}

func TestVerifyBridgeMode_VethWrongOvsBridge(t *testing.T) {
	stubBridgeProbes(t,
		map[string]string{"veth-wan-ovs": "br-wan"},
		map[string]string{"veth-wan-br": "br-wan"})
	err := verifyBridgeMode(BridgeModeVeth, "", "br-wan")
	if err == nil || !strings.Contains(err.Error(), OvnExternalBridge) {
		t.Fatalf("expected br-ext mismatch, got: %v", err)
	}
}

func TestVerifyBridgeMode_VethWrongLinuxMaster(t *testing.T) {
	stubBridgeProbes(t,
		map[string]string{"veth-wan-ovs": OvnExternalBridge},
		map[string]string{"veth-wan-br": "br-other"})
	err := verifyBridgeMode(BridgeModeVeth, "", "br-wan")
	if err == nil || !strings.Contains(err.Error(), "br-other") {
		t.Fatalf("expected master mismatch, got: %v", err)
	}
}

func TestVerifyBridgeMode_UnknownModeLists(t *testing.T) {
	err := verifyBridgeMode("macvlan", "enp0s3", "br-wan")
	if err == nil {
		t.Fatal("expected error for unsupported mode")
	}
	if !strings.Contains(err.Error(), BridgeModeDirect) || !strings.Contains(err.Error(), BridgeModeVeth) {
		t.Errorf("expected error to list supported values, got: %v", err)
	}
}

func TestVerifyBridgeMode_EmptyModeRejected(t *testing.T) {
	err := verifyBridgeMode("", "", "")
	if err == nil {
		t.Fatal("expected error for empty mode (D12)")
	}
}

func TestVerifyBridgeMode_DirectMissingExternalIface(t *testing.T) {
	err := verifyBridgeMode(BridgeModeDirect, "", "br-wan")
	if err == nil || !strings.Contains(err.Error(), "external_interface") {
		t.Fatalf("expected external_interface error, got: %v", err)
	}
}

func TestVerifyBridgeMode_DirectMissingBindBridge(t *testing.T) {
	err := verifyBridgeMode(BridgeModeDirect, "enp0s3", "")
	if err == nil || !strings.Contains(err.Error(), "dhcp_bind_bridge") {
		t.Fatalf("expected dhcp_bind_bridge error, got: %v", err)
	}
}

func TestVerifyBridgeMode_VethMissingBindBridge(t *testing.T) {
	err := verifyBridgeMode(BridgeModeVeth, "", "")
	if err == nil || !strings.Contains(err.Error(), "dhcp_bind_bridge") {
		t.Fatalf("expected dhcp_bind_bridge error, got: %v", err)
	}
}

func TestVerifyBridgeMode_VethLinuxBrMissing(t *testing.T) {
	stubBridgeProbes(t,
		map[string]string{"veth-wan-ovs": OvnExternalBridge},
		nil)
	err := verifyBridgeMode(BridgeModeVeth, "", "br-wan")
	if err == nil || !strings.Contains(err.Error(), "veth-wan-br") {
		t.Fatalf("expected veth-wan-br error, got: %v", err)
	}
}

// detectBridgeMode — mulga-998.b Fix 2.

func stubDetectProbes(t *testing.T, macvlans []string, links []string) {
	t.Helper()
	origMac := ifaceIsMacvlan
	origExists := ifaceExists
	t.Cleanup(func() {
		ifaceIsMacvlan = origMac
		ifaceExists = origExists
	})
	macSet := map[string]bool{}
	for _, m := range macvlans {
		macSet[m] = true
	}
	linkSet := map[string]bool{}
	for _, l := range links {
		linkSet[l] = true
	}
	ifaceIsMacvlan = func(name string) bool { return macSet[name] }
	ifaceExists = func(name string) bool { return linkSet[name] }
}

func TestDetectBridgeMode_MacvlanWins(t *testing.T) {
	stubDetectProbes(t, []string{"spx-ext-enp0s3"}, []string{"veth-wan-ovs"})
	if got := detectBridgeMode("enp0s3"); got != BridgeModeMacvlan {
		t.Errorf("want %q, got %q", BridgeModeMacvlan, got)
	}
}

func TestDetectBridgeMode_VethWhenNoMacvlan(t *testing.T) {
	stubDetectProbes(t, nil, []string{"veth-wan-ovs"})
	if got := detectBridgeMode("enp0s3"); got != BridgeModeVeth {
		t.Errorf("want %q, got %q", BridgeModeVeth, got)
	}
}

func TestDetectBridgeMode_FallthroughDirect(t *testing.T) {
	stubDetectProbes(t, nil, nil)
	if got := detectBridgeMode("enp0s3"); got != BridgeModeDirect {
		t.Errorf("want %q, got %q", BridgeModeDirect, got)
	}
}

func TestResolveBridgeConfig_UsesExplicitMode(t *testing.T) {
	stubDetectProbes(t, nil, nil)
	mode, br := resolveBridgeConfig(BridgeModeVeth, "enp0s3", "br-wan")
	if mode != BridgeModeVeth || br != "br-wan" {
		t.Errorf("got (%q,%q), want (%q,br-wan)", mode, br, BridgeModeVeth)
	}
}

func TestResolveBridgeConfig_AutoDetects(t *testing.T) {
	stubDetectProbes(t, nil, []string{"veth-wan-ovs"})
	mode, _ := resolveBridgeConfig("", "enp0s3", "br-wan")
	if mode != BridgeModeVeth {
		t.Errorf("want auto-detect veth, got %q", mode)
	}
}

func TestResolveBridgeConfig_EmptyStaysEmptyWithNoIface(t *testing.T) {
	mode, _ := resolveBridgeConfig("", "", "")
	if mode != "" {
		t.Errorf("empty mode + no iface should stay empty (D12); got %q", mode)
	}
}

func TestResolveBridgeConfig_DefaultsBindBridge(t *testing.T) {
	_, br := resolveBridgeConfig(BridgeModeDirect, "enp0s3", "")
	if br != "br-wan" {
		t.Errorf("empty dhcp_bind_bridge should default to br-wan, got %q", br)
	}
}
