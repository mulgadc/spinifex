package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
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
// refreshSystemInstanceState
// ---------------------------------------------------------------------------

// TestRefreshSystemInstanceState_NonELBv2IsNoop guards the cheap-path
// short-circuit: customer VMs (ManagedBy="") have no tmpfs blobs to
// regenerate, and reaching the lb-record lookup would error pointlessly.
func TestRefreshSystemInstanceState_NonELBv2IsNoop(t *testing.T) {
	d := &Daemon{vmMgr: vm.NewManager()}
	inst := &vm.VM{ID: "i-customer", ManagedBy: ""}

	err := d.refreshSystemInstanceState(inst)
	require.NoError(t, err, "non-ELBv2 VMs must short-circuit before the service lookup — customer instances do not need fw_cfg blob regeneration")
}

// TestRefreshSystemInstanceState_NilELBv2ServiceIsNoop covers the early
// daemon-startup window where vmMgr.Restore runs before elbv2Service is
// wired. The hook must not panic; the VM stays Pending and a later
// recovery cycle (or operator retry) reaches the regen path with the
// service available.
func TestRefreshSystemInstanceState_NilELBv2ServiceIsNoop(t *testing.T) {
	d := &Daemon{vmMgr: vm.NewManager(), elbv2Service: nil}
	inst := &vm.VM{ID: "i-no-svc", ManagedBy: tags.ManagedByELBv2}

	err := d.refreshSystemInstanceState(inst)
	require.NoError(t, err, "an unwired elbv2Service must NOT block recovery with a hard error — the service finishes wiring later in Daemon.Start")
}

// TestRefreshSystemInstanceState_NoLBRecord verifies the failure-mode path:
// the VM record survived but the LB record was wiped from KV (manual
// admin deletion, KV corruption). Recovery must surface the error so the
// caller flips the instance to recovery_failed rather than booting it
// against guessed inputs.
func TestRefreshSystemInstanceState_NoLBRecord(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)

	_, nc, _ := testutil.StartTestJetStream(t)
	svc, err := handlers_elbv2.NewELBv2ServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	t.Cleanup(svc.Close)

	d := &Daemon{vmMgr: vm.NewManager(), elbv2Service: svc}
	inst := &vm.VM{ID: "i-orphan-alb", ManagedBy: tags.ManagedByELBv2, InstanceType: "sys.micro"}

	err = d.refreshSystemInstanceState(inst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rebuild system instance input")
	assert.Contains(t, err.Error(), "no LB record references instance i-orphan-alb",
		"the wrapped error must identify the missing LB so journal grep finds the orphaned VM record")

	// No tmpfiles must be created when rebuild fails — leaving stale blobs
	// behind would mask the failure on the next recovery attempt.
	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), "fwcfg-i-orphan-alb"),
			"unexpected tmpfile left behind on rebuild failure: %s", e.Name())
	}
}

// TestRefreshSystemInstanceState_RewritesBlobs covers the success path —
// the bug's actual fix. Given an ELBv2-managed VM whose tmpfs blobs were
// wiped (simulated by deleting the files after CreateLoadBalancer), the
// hook must regenerate all three blobs at the deterministic paths the
// persisted vm.Config.FwCfg references.
func TestRefreshSystemInstanceState_RewritesBlobs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)

	_, nc, _ := testutil.StartTestJetStream(t)
	svc, err := handlers_elbv2.NewELBv2ServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	t.Cleanup(svc.Close)

	// Pre-populate an LB record that references the recovery instance. We
	// bypass CreateLoadBalancer (which would require a VPC service + mock
	// launcher) by writing the record straight into the store — the
	// rebuild path's only requirement is that some LB.InstanceID matches.
	record := &handlers_elbv2.LoadBalancerRecord{
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
	}
	// Both store instances share the same JetStream KV bucket, so a Put
	// here is visible to svc.store on the next List call.
	seedStore, err := handlers_elbv2.NewStore(nc)
	require.NoError(t, err)
	require.NoError(t, seedStore.PutLoadBalancer(record))

	d := &Daemon{vmMgr: vm.NewManager(), elbv2Service: svc}
	inst := &vm.VM{
		ID:           "i-recover-blobs",
		ManagedBy:    tags.ManagedByELBv2,
		InstanceType: "sys.micro",
		ENIMac:       "02:aa:bb:cc:dd:01",
		MgmtMAC:      "02:a0:00:11:22:33",
		MgmtIP:       "172.31.0.7",
	}

	require.NoError(t, d.refreshSystemInstanceState(inst))

	// Three blobs at deterministic paths — these are the exact filenames
	// writeFwCfgBlobs bakes into vm.Config.FwCfg, so the persisted QEMU
	// command line will reference the same paths after the rewrite.
	for _, suffix := range []string{"netcfg", "lbenv", "cacert"} {
		path := filepath.Join(tmpDir, fmt.Sprintf("fwcfg-%s-%s.tmp", inst.ID, suffix))
		_, statErr := os.Stat(path)
		assert.NoError(t, statErr, "expected fw_cfg blob at %s after refresh — the persisted -fw_cfg arg in vm.Config references this exact path", path)
	}
}
