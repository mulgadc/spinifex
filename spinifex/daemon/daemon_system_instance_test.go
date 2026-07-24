package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDaemonWithVMs(vms ...*vm.VM) *Daemon {
	d := &Daemon{vmMgr: vm.NewManager()}
	for _, v := range vms {
		d.vmMgr.Insert(v)
	}
	return d
}

func TestWaitForSystemInstance_AlreadyRunning(t *testing.T) {
	d := newDaemonWithVMs(&vm.VM{ID: "i-test1", Status: vm.StateRunning})

	err := d.WaitForSystemInstance("i-test1", 1*time.Second)
	if err != nil {
		t.Fatalf("expected no error for running instance, got: %v", err)
	}
}

func TestWaitForSystemInstance_NotFound(t *testing.T) {
	d := newDaemonWithVMs()

	err := d.WaitForSystemInstance("i-nonexistent", 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for missing instance")
	}
}

func TestWaitForSystemInstance_ErrorState(t *testing.T) {
	d := newDaemonWithVMs(&vm.VM{ID: "i-failed", Status: vm.StateError})

	err := d.WaitForSystemInstance("i-failed", 1*time.Second)
	if err == nil {
		t.Fatal("expected error for failed instance")
	}
}

func TestWaitForSystemInstance_TransitionsToRunning(t *testing.T) {
	inst := &vm.VM{ID: "i-pending", Status: vm.StateProvisioning}
	d := newDaemonWithVMs(inst)

	// Transition to running after a short delay
	go func() {
		time.Sleep(200 * time.Millisecond)
		d.vmMgr.UpdateState(inst.ID, func(v *vm.VM) { v.Status = vm.StateRunning })
	}()

	err := d.WaitForSystemInstance("i-pending", 2*time.Second)
	if err != nil {
		t.Fatalf("expected instance to reach running, got: %v", err)
	}
}

func TestWaitForSystemInstance_Timeout(t *testing.T) {
	d := newDaemonWithVMs(&vm.VM{ID: "i-stuck", Status: vm.StateProvisioning})

	err := d.WaitForSystemInstance("i-stuck", 600*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// ---------------------------------------------------------------------------
// verifySystemInstanceLaunched
// ---------------------------------------------------------------------------

func TestVerifySystemInstanceLaunched_Running(t *testing.T) {
	inst := &vm.VM{ID: "i-up", Status: vm.StateRunning}
	d := newDaemonWithVMs(inst)

	require.NoError(t, d.verifySystemInstanceLaunched(inst))
}

// LaunchRunInstances swallows launch errors and leaves the instance in a
// cleanup state, so every non-running state must surface as a launch failure —
// a GPU claim failure marks the VM shutting-down via MarkFailed.
func TestVerifySystemInstanceLaunched_NonRunningStatesFail(t *testing.T) {
	for _, status := range []vm.InstanceState{
		vm.StateShuttingDown,
		vm.StateError,
		vm.StateTerminated,
		vm.StatePending,
		vm.StateProvisioning,
	} {
		t.Run(string(status), func(t *testing.T) {
			inst := &vm.VM{ID: "i-down", Status: status}
			d := newDaemonWithVMs(inst)

			err := d.verifySystemInstanceLaunched(inst)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "i-down")
			assert.Contains(t, err.Error(), string(status))
		})
	}
}

// MarkFailed transitions synchronously before running its cleanup goroutine, so
// the check sees the failure without racing the teardown.
func TestVerifySystemInstanceLaunched_AfterMarkFailedIsAnError(t *testing.T) {
	inst := &vm.VM{ID: "i-gpu-claim-failed", Status: vm.StateProvisioning}
	d := newDaemonWithVMs(inst)
	d.vmMgr.MarkFailed(context.Background(), inst, "gpu_claim_failed")

	require.Error(t, d.verifySystemInstanceLaunched(inst))
}

// ---------------------------------------------------------------------------
// buildNetcfgBlob
// ---------------------------------------------------------------------------

func TestBuildNetcfgBlob_NoDefault(t *testing.T) {
	nics := []handlers_elbv2.NICConfig{
		{MAC: "02:aa:bb:cc:dd:01", CIDR: "10.0.1.5/24", IsDefault: false},
	}
	_, err := buildNetcfgBlob(nics)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one NIC must have IsDefault=true")
}

func TestBuildNetcfgBlob_TwoDefaults(t *testing.T) {
	nics := []handlers_elbv2.NICConfig{
		{MAC: "02:aa:bb:cc:dd:01", IsDefault: true},
		{MAC: "02:aa:bb:cc:dd:02", IsDefault: true},
	}
	_, err := buildNetcfgBlob(nics)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one NIC must have IsDefault=true, got 2")
}

func TestBuildNetcfgBlob_SingleNICMinimal(t *testing.T) {
	nics := []handlers_elbv2.NICConfig{
		{MAC: "02:aa:bb:cc:dd:01", IsDefault: true},
	}
	got, err := buildNetcfgBlob(nics)
	require.NoError(t, err)
	assert.Contains(t, got, "NIC0_MAC=02:aa:bb:cc:dd:01\n")
	assert.Contains(t, got, "NIC0_DEFAULT=1\n")
	// Optional fields absent when empty.
	assert.NotContains(t, got, "NIC0_CIDR=")
	assert.NotContains(t, got, "NIC0_GW=")
}

func TestBuildNetcfgBlob_FullNIC(t *testing.T) {
	nics := []handlers_elbv2.NICConfig{
		{
			MAC:       "02:aa:bb:cc:dd:01",
			CIDR:      "10.0.1.5/24",
			Gateway:   "10.0.1.1",
			IsDefault: true,
			RouteDst:  "10.20.0.5/32",
			RouteVia:  "10.0.1.254",
		},
	}
	got, err := buildNetcfgBlob(nics)
	require.NoError(t, err)
	assert.Contains(t, got, "NIC0_MAC=02:aa:bb:cc:dd:01\n")
	assert.Contains(t, got, "NIC0_CIDR=10.0.1.5/24\n")
	assert.Contains(t, got, "NIC0_GW=10.0.1.1\n")
	assert.Contains(t, got, "NIC0_DEFAULT=1\n")
	assert.Contains(t, got, "NIC0_ROUTE_DST=10.20.0.5/32\n")
	assert.Contains(t, got, "NIC0_ROUTE_VIA=10.0.1.254\n")
}

func TestBuildNetcfgBlob_MultiNIC_DefaultFlags(t *testing.T) {
	nics := []handlers_elbv2.NICConfig{
		{MAC: "02:aa:bb:cc:dd:01", CIDR: "10.0.1.5/24", Gateway: "10.0.1.1", IsDefault: true},
		{MAC: "02:aa:bb:cc:dd:02", CIDR: "192.168.100.5/24", IsDefault: false},
	}
	got, err := buildNetcfgBlob(nics)
	require.NoError(t, err)
	assert.Contains(t, got, "NIC0_DEFAULT=1\n")
	assert.Contains(t, got, "NIC1_DEFAULT=0\n")
	assert.Contains(t, got, "NIC1_MAC=02:aa:bb:cc:dd:02\n")
}

// ---------------------------------------------------------------------------
// tapNameForNIC
// ---------------------------------------------------------------------------

func TestResolveENIAccount_OwnerWhenSet(t *testing.T) {
	// A cross-account extra ENI attaches under its own account.
	assert.Equal(t, "999988887777", resolveENIAccount("999988887777", "000000000000"))
}

func TestResolveENIAccount_FallsBackWhenEmpty(t *testing.T) {
	// A same-account extra ENI (empty AccountID) inherits the primary account.
	assert.Equal(t, "000000000000", resolveENIAccount("", "000000000000"))
}

func TestTapNameForNIC_PrimaryENI(t *testing.T) {
	input := &handlers_elbv2.SystemInstanceInput{
		ENIID: "eni-0123456789abcdef0",
	}
	got := tapNameForNIC(0, handlers_elbv2.NICConfig{}, "i-abc123", input)
	// TapDeviceName strips "eni-" prefix → "tap0123456789abcde" (15 chars max)
	assert.True(t, strings.HasPrefix(got, "tap"), "expected tap prefix, got %q", got)
	assert.LessOrEqual(t, len(got), 15)
}

func TestTapNameForNIC_MgmtNIC(t *testing.T) {
	input := &handlers_elbv2.SystemInstanceInput{ENIID: "eni-aaa"}
	got := tapNameForNIC(1, handlers_elbv2.NICConfig{}, "i-deadbeef", input)
	// MgmtTapName strips "i-" prefix → "mgdeadbeef" (10 chars)
	assert.True(t, strings.HasPrefix(got, "mg"), "expected mg prefix, got %q", got)
	assert.LessOrEqual(t, len(got), 15)
}

func TestTapNameForNIC_ExtraENI(t *testing.T) {
	input := &handlers_elbv2.SystemInstanceInput{
		ENIID: "eni-primary",
		ExtraENIs: []handlers_elbv2.ExtraENIInput{
			{ENIID: "eni-extra0"},
			{ENIID: "eni-extra1"},
		},
	}
	got := tapNameForNIC(2, handlers_elbv2.NICConfig{}, "i-inst", input)
	assert.True(t, strings.HasPrefix(got, "tap"), "expected tap prefix for extra ENI, got %q", got)
}

func TestTapNameForNIC_ExtraENI_OutOfRange(t *testing.T) {
	input := &handlers_elbv2.SystemInstanceInput{
		ENIID:     "eni-primary",
		ExtraENIs: []handlers_elbv2.ExtraENIInput{},
	}
	// idx=2 → extraIdx=0 but no ExtraENIs → fallback name
	got := tapNameForNIC(2, handlers_elbv2.NICConfig{}, "i-inst", input)
	assert.Equal(t, fmt.Sprintf("tap-unknown-%d", 2), got)
}

// ---------------------------------------------------------------------------
// buildNICNetdevs
// ---------------------------------------------------------------------------

func TestBuildNICNetdevs_SingleNIC(t *testing.T) {
	input := &handlers_elbv2.SystemInstanceInput{
		ENIID: "eni-000000000000000a",
		NICs: []handlers_elbv2.NICConfig{
			{MAC: "02:aa:bb:cc:dd:01", IsDefault: true},
		},
	}
	res := buildNICNetdevs("i-test", input, microvmMachineType())
	require.Len(t, res.netdevs, 1)
	require.Len(t, res.devices, 1)
	assert.Contains(t, res.netdevs[0].Value, "tap,id=net0,")
	// microvm machine type → virtio-net-device transport
	assert.Contains(t, res.devices[0].Value, "virtio-net-device,netdev=net0")
	assert.Contains(t, res.devices[0].Value, "02:aa:bb:cc:dd:01")
}

func TestBuildNICNetdevs_TwoNICs(t *testing.T) {
	input := &handlers_elbv2.SystemInstanceInput{
		ENIID: "eni-000000000000000b",
		NICs: []handlers_elbv2.NICConfig{
			{MAC: "02:aa:bb:cc:dd:01", IsDefault: true},
			{MAC: "02:aa:bb:cc:dd:02", IsDefault: false},
		},
	}
	res := buildNICNetdevs("i-test2", input, microvmMachineType())
	require.Len(t, res.netdevs, 2)
	require.Len(t, res.devices, 2)
	assert.Contains(t, res.netdevs[0].Value, "id=net0,")
	assert.Contains(t, res.netdevs[1].Value, "id=net1,")
	assert.Contains(t, res.devices[0].Value, "netdev=net0")
	assert.Contains(t, res.devices[1].Value, "netdev=net1")
}

func TestBuildNICNetdevs_EmptyNICs(t *testing.T) {
	input := &handlers_elbv2.SystemInstanceInput{ENIID: "eni-empty"}
	res := buildNICNetdevs("i-empty", input, microvmMachineType())
	assert.Empty(t, res.netdevs)
	assert.Empty(t, res.devices)
}

func TestBuildNICNetdevs_UnprovisionedMgmtNIC_Skipped(t *testing.T) {
	// Single-node host with no br-mgmt: NIC[1] (mgmt) never got a MAC/tap.
	input := &handlers_elbv2.SystemInstanceInput{
		ENIID: "eni-000000000000000c",
		NICs: []handlers_elbv2.NICConfig{
			{MAC: "02:aa:bb:cc:dd:01", IsDefault: true},
			{MAC: ""},
		},
	}
	res := buildNICNetdevs("i-test3", input, microvmMachineType())
	require.Len(t, res.netdevs, 1)
	require.Len(t, res.devices, 1)
	assert.Contains(t, res.netdevs[0].Value, "id=net0,")
	assert.Contains(t, res.devices[0].Value, "netdev=net0")
}

func TestBuildNICNetdevs_ProvisionedMgmtNIC_Included(t *testing.T) {
	// Multi-node host with br-mgmt: NIC[1] (mgmt) has a real MAC/tap.
	input := &handlers_elbv2.SystemInstanceInput{
		ENIID: "eni-000000000000000d",
		NICs: []handlers_elbv2.NICConfig{
			{MAC: "02:aa:bb:cc:dd:01", IsDefault: true},
			{MAC: "02:aa:bb:cc:dd:02"},
		},
	}
	res := buildNICNetdevs("i-test4", input, microvmMachineType())
	require.Len(t, res.netdevs, 2)
	require.Len(t, res.devices, 2)
	assert.Contains(t, res.netdevs[0].Value, "id=net0,")
	assert.Contains(t, res.netdevs[1].Value, "id=net1,")
	assert.Contains(t, res.devices[1].Value, "netdev=net1")
}

func TestBuildNICNetdevs_UnprovisionedMgmtNIC_ExtraENIUnaffected(t *testing.T) {
	// NIC[1] mgmt unprovisioned + NIC[2] extra ENI provisioned: net1 must be
	// skipped but the extra ENI's tap name resolution (index-based) must not
	// be shifted/renumbered.
	input := &handlers_elbv2.SystemInstanceInput{
		ENIID: "eni-primary000000000",
		ExtraENIs: []handlers_elbv2.ExtraENIInput{
			{ENIID: "eni-extra000000000000"},
		},
		NICs: []handlers_elbv2.NICConfig{
			{MAC: "02:aa:bb:cc:dd:01", IsDefault: true},
			{MAC: ""},
			{MAC: "02:aa:bb:cc:dd:03"},
		},
	}
	res := buildNICNetdevs("i-test5", input, microvmMachineType())
	require.Len(t, res.netdevs, 2)
	require.Len(t, res.devices, 2)
	assert.Contains(t, res.netdevs[0].Value, "id=net0,")
	// net1 (mgmt) skipped entirely — net2 (extra ENI) keeps its own ID.
	assert.Contains(t, res.netdevs[1].Value, "id=net2,")
	assert.Contains(t, res.devices[1].Value, "netdev=net2")
	assert.Contains(t, res.devices[1].Value, "02:aa:bb:cc:dd:03")
	// Extra ENI's tap name must still resolve against ExtraENIs[0], not be
	// mis-mapped by the gap at index 1.
	assert.Equal(t, vm.TapDeviceName("eni-extra000000000000"), tapNameForNIC(2, input.NICs[2], "i-test5", input))
}

// ---------------------------------------------------------------------------
// nicProvisioned
// ---------------------------------------------------------------------------

func TestNicProvisioned_MgmtSlotEmptyMAC(t *testing.T) {
	assert.False(t, nicProvisioned(1, handlers_elbv2.NICConfig{MAC: ""}))
}

func TestNicProvisioned_MgmtSlotWithMAC(t *testing.T) {
	assert.True(t, nicProvisioned(1, handlers_elbv2.NICConfig{MAC: "02:aa:bb:cc:dd:02"}))
}

func TestNicProvisioned_NonMgmtSlotsAlwaysTrue(t *testing.T) {
	// Only the mgmt slot (index 1) is ever gated on MAC presence — an empty
	// MAC anywhere else (e.g. primary, not expected in practice) is not
	// treated as "unprovisioned" by this guard.
	assert.True(t, nicProvisioned(0, handlers_elbv2.NICConfig{MAC: ""}))
	assert.True(t, nicProvisioned(2, handlers_elbv2.NICConfig{MAC: ""}))
}

// ---------------------------------------------------------------------------
// microvmMachineType
// ---------------------------------------------------------------------------

func TestMicrovmMachineType(t *testing.T) {
	mt := microvmMachineType()
	assert.Contains(t, mt, "microvm,")
	assert.Contains(t, mt, "isa-serial=on")
}

// ---------------------------------------------------------------------------
// writeFwCfgBlobs
// ---------------------------------------------------------------------------

func TestWriteFwCfgBlobs_HappyPath(t *testing.T) {
	// Use a temp dir as runtime dir so files land somewhere writable.
	tmpDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)

	d := &Daemon{}
	instanceID := "i-fwcfg-test"
	input := &handlers_elbv2.SystemInstanceInput{
		NICs: []handlers_elbv2.NICConfig{
			{MAC: "02:aa:bb:cc:dd:01", CIDR: "10.0.1.5/24", Gateway: "10.0.1.1", IsDefault: true},
		},
		LBAgentEnv: "LB_BACKEND=10.0.1.10:8080\n",
		CACert:     "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n",
	}

	entries, err := d.writeFwCfgBlobs(instanceID, input)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	// Check names are as expected.
	assert.Equal(t, "opt/spinifex/netcfg", entries[0].Name)
	assert.Equal(t, "opt/spinifex/lb-agent-env", entries[1].Name)
	assert.Equal(t, "opt/spinifex/ca-cert", entries[2].Name)

	// Verify files were written and contain expected content.
	netcfgData, err := os.ReadFile(entries[0].File)
	require.NoError(t, err)
	assert.Contains(t, string(netcfgData), "NIC0_MAC=02:aa:bb:cc:dd:01")

	lbenvData, err := os.ReadFile(entries[1].File)
	require.NoError(t, err)
	assert.Equal(t, "LB_BACKEND=10.0.1.10:8080\n", string(lbenvData))

	cacertData, err := os.ReadFile(entries[2].File)
	require.NoError(t, err)
	assert.Contains(t, string(cacertData), "BEGIN CERTIFICATE")

	// Cleanup: remove the tmpfiles (proves they are real filesystem files).
	for _, e := range entries {
		assert.NoError(t, os.Remove(e.File))
	}
}

func TestWriteFwCfgBlobs_UnprovisionedMgmtNIC_ExcludedFromGuestConfig(t *testing.T) {
	// Single-node host with no br-mgmt: the mgmt NIC (index 1) never got a
	// MAC/CIDR. The guest netcfg blob must not carry a NIC1 entry at all.
	tmpDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)

	d := &Daemon{}
	instanceID := "i-fwcfg-nomgmt"
	input := &handlers_elbv2.SystemInstanceInput{
		NICs: []handlers_elbv2.NICConfig{
			{MAC: "02:aa:bb:cc:dd:01", CIDR: "10.0.1.5/24", Gateway: "10.0.1.1", IsDefault: true},
			{MAC: ""},
		},
	}

	entries, err := d.writeFwCfgBlobs(instanceID, input)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	netcfgData, err := os.ReadFile(entries[0].File)
	require.NoError(t, err)
	assert.Contains(t, string(netcfgData), "NIC0_MAC=02:aa:bb:cc:dd:01")
	// The unprovisioned mgmt NIC must not appear as NIC1 (nor anywhere else).
	assert.NotContains(t, string(netcfgData), "NIC1_")
	assert.NotContains(t, string(netcfgData), "MAC=\n")

	for _, e := range entries {
		assert.NoError(t, os.Remove(e.File))
	}
}

func TestWriteFwCfgBlobs_ProvisionedMgmtNIC_IncludedInGuestConfig(t *testing.T) {
	// Multi-node host with br-mgmt: mgmt NIC has a real MAC/CIDR and must
	// still be wired into the guest config exactly as today.
	tmpDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)

	d := &Daemon{}
	instanceID := "i-fwcfg-mgmt"
	input := &handlers_elbv2.SystemInstanceInput{
		NICs: []handlers_elbv2.NICConfig{
			{MAC: "02:aa:bb:cc:dd:01", CIDR: "10.0.1.5/24", Gateway: "10.0.1.1", IsDefault: true},
			{MAC: "02:aa:bb:cc:dd:02", CIDR: "192.168.100.5/24"},
		},
	}

	entries, err := d.writeFwCfgBlobs(instanceID, input)
	require.NoError(t, err)

	netcfgData, err := os.ReadFile(entries[0].File)
	require.NoError(t, err)
	assert.Contains(t, string(netcfgData), "NIC0_MAC=02:aa:bb:cc:dd:01")
	assert.Contains(t, string(netcfgData), "NIC1_MAC=02:aa:bb:cc:dd:02")

	for _, e := range entries {
		assert.NoError(t, os.Remove(e.File))
	}
}

func TestWriteFwCfgBlobs_InvalidNICs_NoDefault(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)

	d := &Daemon{}
	input := &handlers_elbv2.SystemInstanceInput{
		NICs: []handlers_elbv2.NICConfig{
			{MAC: "02:aa:bb:cc:dd:01", IsDefault: false},
		},
	}

	_, err := d.writeFwCfgBlobs("i-bad-nics", input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IsDefault")

	// No tmpfiles must be left behind.
	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), "fwcfg-i-bad-nics"),
			"unexpected tmpfile left behind: %s", e.Name())
	}
}

// ---------------------------------------------------------------------------
// buildDirectBootConfig — mmio slot packing
// ---------------------------------------------------------------------------

func TestBuildDirectBootConfig_MmioSlotsPacked_MgmtSkipped(t *testing.T) {
	// NIC[1] (mgmt) unprovisioned (no br-mgmt) must not reserve an mmio slot;
	// remaining NICs (primary, extra ENI) get packed, contiguous slots.
	tmpDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)

	d := &Daemon{resourceMgr: &ResourceManager{instanceTypes: map[string]*ec2.InstanceTypeInfo{}}}
	input := &handlers_elbv2.SystemInstanceInput{
		ENIID: "eni-mmio000000000001",
		ExtraENIs: []handlers_elbv2.ExtraENIInput{
			{ENIID: "eni-mmio-extra00000001"},
		},
		NICs: []handlers_elbv2.NICConfig{
			{MAC: "02:aa:bb:cc:dd:01", IsDefault: true},
			{MAC: ""},
			{MAC: "02:aa:bb:cc:dd:03"},
		},
	}

	cfg, err := d.buildDirectBootConfig("i-mmio-test", input)
	require.NoError(t, err)

	// Only 2 real devices (mgmt skipped) → slots packed at 0 and 1, not 0 and 2.
	assert.Contains(t, cfg.KernelCmdline, "virtio_mmio.device=0x200@0xfeb00000:5")
	assert.Contains(t, cfg.KernelCmdline, "virtio_mmio.device=0x200@0xfeb00200:6")
	assert.NotContains(t, cfg.KernelCmdline, "virtio_mmio.device=0x200@0xfeb00400:7")

	require.Len(t, cfg.NetDevs, 2)
	require.Len(t, cfg.Devices, 2)

	for _, e := range cfg.FwCfg {
		assert.NoError(t, os.Remove(e.File))
	}
}

func TestBuildDirectBootConfig_MmioSlotsUnchanged_MgmtProvisioned(t *testing.T) {
	// Multi-node host with br-mgmt: all NICs provisioned, slots stay 1:1
	// with NIC index as before this fix.
	tmpDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)

	d := &Daemon{resourceMgr: &ResourceManager{instanceTypes: map[string]*ec2.InstanceTypeInfo{}}}
	input := &handlers_elbv2.SystemInstanceInput{
		ENIID: "eni-mmio000000000002",
		NICs: []handlers_elbv2.NICConfig{
			{MAC: "02:aa:bb:cc:dd:01", IsDefault: true},
			{MAC: "02:aa:bb:cc:dd:02"},
		},
	}

	cfg, err := d.buildDirectBootConfig("i-mmio-test2", input)
	require.NoError(t, err)

	assert.Contains(t, cfg.KernelCmdline, "virtio_mmio.device=0x200@0xfeb00000:5")
	assert.Contains(t, cfg.KernelCmdline, "virtio_mmio.device=0x200@0xfeb00200:6")
	require.Len(t, cfg.NetDevs, 2)
	require.Len(t, cfg.Devices, 2)

	for _, e := range cfg.FwCfg {
		assert.NoError(t, os.Remove(e.File))
	}
}

// ---------------------------------------------------------------------------
// refreshSystemInstanceState
// ---------------------------------------------------------------------------

// newRefreshTestDaemon returns a Daemon wired with vmMgr + a fresh ELBv2
// service, with XDG_RUNTIME_DIR pointed at a per-test tmpdir so
// writeFwCfgBlobs is sandboxed. The returned nats.Conn lets the caller seed
// LB records via a separate Store handle sharing the same KV bucket.
func newRefreshTestDaemon(t *testing.T) (*Daemon, *nats.Conn, string) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)
	_, nc, _ := testutil.StartTestJetStream(t)
	svc, err := handlers_elbv2.NewELBv2ServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	t.Cleanup(svc.Close)
	return &Daemon{vmMgr: vm.NewManager(), elbv2Service: svc}, nc, tmpDir
}

func TestRefreshSystemInstanceState_NonELBv2IsNoop(t *testing.T) {
	d := &Daemon{vmMgr: vm.NewManager()}
	require.NoError(t, d.refreshSystemInstanceState(&vm.VM{ID: "i-customer", ManagedBy: ""}))
}

func TestRefreshSystemInstanceState_NilELBv2ServiceErrors(t *testing.T) {
	d := &Daemon{vmMgr: vm.NewManager(), elbv2Service: nil}
	err := d.refreshSystemInstanceState(&vm.VM{ID: "i-no-svc", ManagedBy: tags.ManagedByELBv2})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "elbv2 service unavailable")
}

func TestRefreshSystemInstanceState_NoLBRecord(t *testing.T) {
	d, _, tmpDir := newRefreshTestDaemon(t)
	inst := &vm.VM{ID: "i-orphan-alb", ManagedBy: tags.ManagedByELBv2, InstanceType: "sys.micro"}

	err := d.refreshSystemInstanceState(inst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rebuild system instance input")
	assert.Contains(t, err.Error(), "no LB record references instance i-orphan-alb")

	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), "fwcfg-i-orphan-alb"),
			"stale tmpfile left behind on rebuild failure: %s", e.Name())
	}
}

func TestRefreshSystemInstanceState_RewritesBlobs(t *testing.T) {
	d, nc, tmpDir := newRefreshTestDaemon(t)
	d.elbv2Service.SystemAccessKey = "AKID-recover"
	d.elbv2Service.SystemSecretKey = "SECRET-recover"
	d.elbv2Service.GatewayURL = "https://10.0.0.1:9999"
	d.elbv2Service.CACert = "-----BEGIN CERTIFICATE-----\nfake-ca\n-----END CERTIFICATE-----\n"

	seedStore, err := handlers_elbv2.NewStore(t.Context(), nc)
	require.NoError(t, err)
	require.NoError(t, seedStore.PutLoadBalancer(t.Context(), &handlers_elbv2.LoadBalancerRecord{
		LoadBalancerID: "lb-recover",
		Name:           "recover-alb",
		Scheme:         handlers_elbv2.SchemeInternal,
		Type:           handlers_elbv2.LoadBalancerTypeApplication,
		State:          handlers_elbv2.StateActive,
		Subnets:        []string{"subnet-aaa"},
		ENIs:           []string{"eni-primary"},
		InstanceID:     "i-recover-blobs",
		VPCIP:          "10.0.1.5",
		AccountID:      "123456789012",
	}))

	inst := &vm.VM{
		ID:           "i-recover-blobs",
		ManagedBy:    tags.ManagedByELBv2,
		InstanceType: "sys.micro",
		ENIMac:       "02:aa:bb:cc:dd:01",
		MgmtMAC:      "02:a0:00:11:22:33",
		MgmtIP:       "172.31.0.7",
	}
	require.NoError(t, d.refreshSystemInstanceState(inst))

	netcfg, err := os.ReadFile(filepath.Join(tmpDir, "fwcfg-i-recover-blobs-netcfg.tmp"))
	require.NoError(t, err)
	assert.Contains(t, string(netcfg), "NIC0_MAC=02:aa:bb:cc:dd:01")
	assert.Contains(t, string(netcfg), "NIC1_MAC=02:a0:00:11:22:33")

	lbenv, err := os.ReadFile(filepath.Join(tmpDir, "fwcfg-i-recover-blobs-lbenv.tmp"))
	require.NoError(t, err)
	assert.Contains(t, string(lbenv), "LB_LB_ID=lb-recover")
	assert.Contains(t, string(lbenv), "LB_ACCESS_KEY=AKID-recover")

	cacert, err := os.ReadFile(filepath.Join(tmpDir, "fwcfg-i-recover-blobs-cacert.tmp"))
	require.NoError(t, err)
	assert.Contains(t, string(cacert), "fake-ca")
}

// TestRefreshSystemInstanceState_WriteErrorPropagates exercises the
// writeFwCfgBlobs error path (plan §3): a write failure under the tmpfs
// path must surface as a wrapped error so MarkRecoveryFailed gets called.
func TestRefreshSystemInstanceState_WriteErrorPropagates(t *testing.T) {
	d, nc, tmpDir := newRefreshTestDaemon(t)

	seedStore, err := handlers_elbv2.NewStore(t.Context(), nc)
	require.NoError(t, err)
	require.NoError(t, seedStore.PutLoadBalancer(t.Context(), &handlers_elbv2.LoadBalancerRecord{
		LoadBalancerID: "lb-werr",
		Name:           "werr-alb",
		Scheme:         handlers_elbv2.SchemeInternal,
		Type:           handlers_elbv2.LoadBalancerTypeApplication,
		State:          handlers_elbv2.StateActive,
		Subnets:        []string{"subnet-aaa"},
		ENIs:           []string{"eni-primary"},
		InstanceID:     "i-werr",
		VPCIP:          "10.0.1.5",
		AccountID:      "123456789012",
	}))

	// Replace XDG_RUNTIME_DIR with a regular file so os.WriteFile in
	// writeFwCfgBlobs fails with ENOTDIR. tmpDir from the helper is no
	// longer the runtime dir for this test.
	_ = tmpDir
	notADir := filepath.Join(t.TempDir(), "blocking-file")
	require.NoError(t, os.WriteFile(notADir, nil, 0600))
	t.Setenv("XDG_RUNTIME_DIR", notADir)

	inst := &vm.VM{
		ID:           "i-werr",
		ManagedBy:    tags.ManagedByELBv2,
		InstanceType: "sys.micro",
		ENIMac:       "02:aa:bb:cc:dd:01",
		MgmtMAC:      "02:a0:00:11:22:33",
		MgmtIP:       "172.31.0.7",
	}
	err = d.refreshSystemInstanceState(inst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rewrite fw_cfg blobs")
}

// TestLaunchSystemInstance_NATFailureRollsBackPublicIP regresses the silent
// corruption where an internet-facing ALB launch committed the IPAM
// allocation and surfaced the public IP on the ENI record even when
// vpc.add-nat failed — the OVN dnat_and_snat rule was never created and the
// public IP black-holed with no reconciler. With the fix, NAT NACK must roll
// back the ENI public IP and release the IPAM lease before returning.
func TestLaunchSystemInstance_NATFailureRollsBackPublicIP(t *testing.T) {
	d := createVPCTestDaemon(t)

	// Wire an ExternalIPAM against a fresh JS server. The pool gives us a
	// known IP that the rollback must release back.
	jsNS, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	require.NoError(t, err)
	go jsNS.Start()
	require.True(t, jsNS.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { jsNS.Shutdown() })

	jsNC, err := nats.Connect(jsNS.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { jsNC.Close() })

	js, err := jsNC.JetStream()
	require.NoError(t, err)
	ipam, err := handlers_ec2_vpc.NewExternalIPAM(js, []external.ExternalPoolConfig{
		{Name: "wan-test", RangeStart: "203.0.113.10", RangeEnd: "203.0.113.20", Gateway: "203.0.113.1", PrefixLen: 24},
	})
	require.NoError(t, err)
	d.externalIPAM = ipam

	// Create a VPC + subnet + ENI so LaunchSystemInstance has a real ENI to
	// attach to and to update with the public IP.
	vpcOut, err := d.vpcService.CreateVpc(t.Context(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.50.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)
	subnetOut, err := d.vpcService.CreateSubnet(t.Context(), &ec2.CreateSubnetInput{
		VpcId:     vpcOut.Vpc.VpcId,
		CidrBlock: aws.String("10.50.1.0/24"),
	}, testAccountID)
	require.NoError(t, err)
	eniOut, err := d.vpcService.CreateNetworkInterface(t.Context(), &ec2.CreateNetworkInterfaceInput{
		SubnetId:    subnetOut.Subnet.SubnetId,
		Description: aws.String("nat-fail-test"),
	}, testAccountID)
	require.NoError(t, err)
	eniID := aws.StringValue(eniOut.NetworkInterface.NetworkInterfaceId)
	eniMac := aws.StringValue(eniOut.NetworkInterface.MacAddress)
	eniIP := aws.StringValue(eniOut.NetworkInterface.PrivateIpAddress)

	// Stand up a vpcd-shaped NACK responder on the daemon's NATS conn —
	// utils.AddNAT publishes on d.natsConn, so the responder must live there.
	sub, err := d.natsConn.Subscribe("vpc.add-nat", func(msg *nats.Msg) {
		_ = msg.Respond([]byte(`{"success":false,"error":"northd unavailable"}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Capture vpc.delete-nat so we can assert the rollback neutralises any
	// rule vpcd may have committed after the AddNAT timeout window.
	deleteNATCh := make(chan map[string]string, 1)
	delSub, err := d.natsConn.Subscribe("vpc.delete-nat", func(msg *nats.Msg) {
		var p map[string]string
		_ = json.Unmarshal(msg.Data, &p)
		select {
		case deleteNATCh <- p:
		default:
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = delSub.Unsubscribe() })

	input := &handlers_elbv2.SystemInstanceInput{
		InstanceType: getTestInstanceType(t),
		SubnetID:     aws.StringValue(subnetOut.Subnet.SubnetId),
		Scheme:       handlers_elbv2.SchemeInternetFacing,
		AccountID:    testAccountID,
		ENIID:        eniID,
		ENIMac:       eniMac,
		ENIIP:        eniIP,
	}

	out, err := d.LaunchSystemInstance(input)
	require.Error(t, err, "NAT NACK must fail the launch")
	assert.Nil(t, out, "no SystemInstanceOutput should be returned on NAT failure")
	assert.Contains(t, err.Error(), "northd unavailable", "error must propagate the vpcd reason")

	// ENI public IP record must be cleared so subsequent DescribeNetworkInterfaces
	// does not surface the unreachable address.
	descOut, err := d.vpcService.DescribeNetworkInterfaces(t.Context(), &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(eniID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, descOut.NetworkInterfaces, 1)
	// Association is built only when the underlying record has a public IP;
	// rollback clears that field, so the helper omits Association entirely.
	assert.Nil(t, descOut.NetworkInterfaces[0].Association,
		"ENI must not carry the unreachable public IP after rollback")

	// IPAM lease must be back in the pool so the next allocation can reuse
	// it. .10 is the gateway (reserved at init); the first allocable IP is
	// .11, which is what the launch grabbed and the rollback released.
	rec, err := ipam.GetPoolRecord("wan-test")
	require.NoError(t, err)
	_, stillAllocated := rec.Allocated["203.0.113.11"]
	assert.False(t, stillAllocated, "rollback must release the allocated IP back to the pool")

	// vpc.delete-nat must be published with the same external IP that AddNAT
	// targeted so vpcd reaps any rule committed after the request timeout.
	select {
	case got := <-deleteNATCh:
		assert.Equal(t, "203.0.113.11", got["external_ip"])
		assert.Equal(t, eniIP, got["logical_ip"])
		assert.Equal(t, "port-"+eniID, got["port_name"])
	case <-time.After(time.Second):
		t.Fatal("rollback must publish vpc.delete-nat to neutralise a half-committed rule")
	}
}

// releaseSystemInstanceEIP must release an eipService-allocated address back to
// its pool and clear the instance's EIP fields. The vm.Manager teardown
// (Terminate/MarkFailed → ReleasePublicIP) only knows the externalIPAM path, so
// without this an internet-facing system VM's allocated+associated EIP would
// leak when its instance is torn down.
func TestReleaseSystemInstanceEIP_ReleasesEipServiceAllocation(t *testing.T) {
	d := createVPCTestDaemon(t)

	jsNS, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	require.NoError(t, err)
	go jsNS.Start()
	require.True(t, jsNS.ReadyForConnections(5*time.Second))
	t.Cleanup(func() { jsNS.Shutdown() })

	jsNC, err := nats.Connect(jsNS.ClientURL())
	require.NoError(t, err)
	t.Cleanup(func() { jsNC.Close() })

	js, err := jsNC.JetStream()
	require.NoError(t, err)
	ipam, err := handlers_ec2_vpc.NewExternalIPAM(js, []external.ExternalPoolConfig{
		{Name: "wan-test", RangeStart: "203.0.113.10", RangeEnd: "203.0.113.20", Gateway: "203.0.113.1", PrefixLen: 24},
	})
	require.NoError(t, err)
	d.externalIPAM = ipam

	eipSvc, err := handlers_ec2_eip.NewEIPServiceImpl(jsNC, ipam, d.vpcService)
	require.NoError(t, err)
	d.eipService = eipSvc

	// Allocate an EIP the way an internet-facing LB VM launch does, then attach
	// it to a registered system instance.
	alloc, err := eipSvc.AllocateAddress(t.Context(), &ec2.AllocateAddressInput{}, testAccountID)
	require.NoError(t, err)
	allocID := aws.StringValue(alloc.AllocationId)
	publicIP := aws.StringValue(alloc.PublicIp)
	require.NotEmpty(t, allocID)
	require.NotEmpty(t, publicIP)

	inst := &vm.VM{ID: "i-lbvm", AccountID: testAccountID, PublicIP: publicIP, PublicIPAllocID: allocID}
	d.vmMgr.Insert(inst)

	rec, err := ipam.GetPoolRecord("wan-test")
	require.NoError(t, err)
	_, allocated := rec.Allocated[publicIP]
	require.True(t, allocated, "EIP is held by the pool before release")

	d.releaseSystemInstanceEIP(inst)

	rec, err = ipam.GetPoolRecord("wan-test")
	require.NoError(t, err)
	_, stillAllocated := rec.Allocated[publicIP]
	assert.False(t, stillAllocated, "release must return the EIP to the pool")

	assert.Empty(t, inst.PublicIP, "instance public IP must be cleared")
	assert.Empty(t, inst.PublicIPAllocID, "instance alloc ID must be cleared so externalIPAM teardown does not double-release")
	assert.Empty(t, inst.PublicIPAssocID)
}
