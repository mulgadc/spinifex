package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon/mgmtipstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readLaunchRecord(t *testing.T, d *Daemon, id string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(d.mgmtLaunchDir(), id+".json"))
	require.NoError(t, err)
	var rec map[string]any
	require.NoError(t, json.Unmarshal(raw, &rec))
	return rec
}

func TestLockMgmtLaunchDir_SecondDaemonOnTheSameStateFails(t *testing.T) {
	dir := t.TempDir()
	first := &Daemon{config: &config.Config{DataDir: dir}}
	second := &Daemon{config: &config.Config{DataDir: dir}}

	require.NoError(t, first.lockMgmtLaunchDir())
	t.Cleanup(func() { _ = first.mgmtLaunchLock.Close() })
	err := second.lockMgmtLaunchDir()

	require.ErrorIs(t, err, ErrMgmtLaunchLocked)
	assert.Nil(t, second.mgmtLaunchLock)
}

func TestMgmtIPAllocator_AllocateNewReportsOnlyACreatedReservation(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.10", "i-old", "node-a"))
	a := newDurableAllocator(t, jsm, "node-a")

	ip, inserted, err := a.AllocateNew("i-new")
	require.NoError(t, err)
	assert.True(t, inserted)
	assert.Equal(t, "10.15.8.11", ip)

	ip, inserted, err = a.AllocateNew("i-old")
	require.NoError(t, err)
	assert.False(t, inserted, "an existing reservation is not a new instance")
	assert.Equal(t, "10.15.8.10", ip)
}

func TestMgmtLaunchAddresses_ClaimsOnlyAFreeAddress(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.10", "i-b", "node-a"))
	addrs := mgmtLaunchAddresses{a: newDurableAllocator(t, jsm, "node-a")}

	require.ErrorIs(t, addrs.Claim("i-a", "10.15.8.10"), mgmtipstore.ErrAddressHeld)
	require.NoError(t, addrs.Claim("i-a", "10.15.8.20"))
	assert.ElementsMatch(t, []mgmtipstore.Entry{
		durableEntry("10.15.8.10", "i-b", "node-a"), durableEntry("10.15.8.20", "i-a", "node-a"),
	}, durableEntries(t, store))

	require.NoError(t, addrs.Release("i-a", "10.15.8.20"))
	assert.Equal(t, []mgmtipstore.Entry{durableEntry("10.15.8.10", "i-b", "node-a")}, durableEntries(t, store))
}

func TestCleanupMgmtNetwork_DurableModeLeavesTheReleaseToTheManager(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.10", "i-a", "node-a"))
	a := newDurableAllocator(t, jsm, "node-a")
	_, err := a.Allocate("i-a")
	require.NoError(t, err)
	d := &Daemon{mgmtIPAllocator: a, networkPlumber: &recordingPlumber{}}

	newInstanceCleanerAdapter(d).CleanupMgmtNetwork(&vm.VM{ID: "i-a", MgmtIP: "10.15.8.10"})

	assert.Equal(t, []mgmtipstore.Entry{durableEntry("10.15.8.10", "i-a", "node-a")}, durableEntries(t, store))
}

// A new system VM on the real launch path gets its first launch record once its
// reservation is created.
func TestLaunchSystemInstance_DurableNewInstanceWritesItsFirstLaunchRecord(t *testing.T) {
	f := newSysInstFixture(t)
	d := f.d
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm)
	d.mgmtIPAllocator = newDurableAllocator(t, jsm, d.node)
	d.mgmtBridgeIP = "10.15.8.1"
	d.networkPlumber = &recordingPlumber{}
	d.vmMgr.SetDeps(vm.Deps{
		NodeID: d.node,
		VolumeMounter: &hookVolumeMounter{onMount: func(v *vm.VM) {
			d.vmMgr.UpdateState(v.ID, func(x *vm.VM) { x.Status = vm.StateRunning })
		}},
		VolumeStateUpdater: d.volumeService,
		MgmtLaunch:         d.mgmtLaunchDeps(),
	})

	in := f.input(f.newENI(t))
	in.Scheme = elbv2domain.SchemeInternal
	in.NICs = append(in.NICs, elbv2domain.NICConfig{})
	out, err := d.LaunchSystemInstance(in)
	require.NoError(t, err)

	rec := readLaunchRecord(t, d, out.InstanceID)
	assert.InDelta(t, 1, rec["generation"], 0)
	assert.Equal(t, d.node, rec["node"])
	assert.Equal(t, "10.15.8.10", rec["mgmt_ip"])
	assert.Equal(t, []mgmtipstore.Entry{durableEntry("10.15.8.10", out.InstanceID, d.node)}, durableEntries(t, store))
}

// An instance whose reservation already exists is not new, so it cannot start
// its launch history over.
func TestAttachSystemMgmtNIC_ExistingReservationWritesNoLaunchRecord(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.10", "i-reused", "node-a"))
	d := &Daemon{
		config:          &config.Config{DataDir: t.TempDir()},
		mgmtBridgeIP:    "10.15.8.1",
		mgmtIPAllocator: newDurableAllocator(t, jsm, "node-a"),
		networkPlumber:  &recordingPlumber{},
		vmMgr:           vm.NewManager(),
	}
	d.vmMgr.SetDeps(vm.Deps{NodeID: "node-a", MgmtLaunch: d.mgmtLaunchDeps()})

	require.NoError(t, d.attachSystemMgmtNIC(&vm.VM{ID: "i-reused"}))

	_, err := os.Stat(filepath.Join(d.mgmtLaunchDir(), "i-reused.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

type failingTapPlumber struct{ recordingPlumber }

func (p *failingTapPlumber) SetupTap(vm.TapSpec) error { return errors.New("tap refused") }

// A failed tap returns only an address this launch created; an existing
// reservation stays held because nothing proves its earlier process stopped.
func TestAttachSystemMgmtNIC_TapFailureReleasesOnlyACreatedReservation(t *testing.T) {
	jsm := newTestMgmtJSM(t)
	store := createDurableMgmtBucket(t, jsm, durableEntry("10.15.8.10", "i-reused", "node-a"))
	d := &Daemon{
		config:          &config.Config{DataDir: t.TempDir()},
		mgmtBridgeIP:    "10.15.8.1",
		mgmtIPAllocator: newDurableAllocator(t, jsm, "node-a"),
		networkPlumber:  &failingTapPlumber{},
		vmMgr:           vm.NewManager(),
	}
	d.vmMgr.SetDeps(vm.Deps{NodeID: "node-a", MgmtLaunch: d.mgmtLaunchDeps()})

	require.Error(t, d.attachSystemMgmtNIC(&vm.VM{ID: "i-reused"}))
	require.Error(t, d.attachSystemMgmtNIC(&vm.VM{ID: "i-new"}))

	assert.Equal(t, []mgmtipstore.Entry{durableEntry("10.15.8.10", "i-reused", "node-a")}, durableEntries(t, store))
}

// A mode the daemon does not know names no allocator, so startup refuses it
// rather than guessing which record guards the node's addresses.
func TestValidateMgmtIPAMMode_RefusesUnknownMode(t *testing.T) {
	for _, mode := range []string{"", MgmtIPAMLegacy, MgmtIPAMDurable} {
		assert.NoError(t, validateMgmtIPAMMode(mode), mode)
	}
	assert.Error(t, validateMgmtIPAMMode("Durable"))
}

// Without a durable allocator, a node configured for anything but legacy keeps
// launch records and no address record, so every system VM launch refuses.
func TestMgmtLaunchDeps_NoDurableAllocatorFailsClosed(t *testing.T) {
	for _, mode := range []string{MgmtIPAMDurable, "durble"} {
		d := &Daemon{config: &config.Config{DataDir: t.TempDir(), Daemon: config.DaemonConfig{MgmtIPAM: mode}}}
		deps := d.mgmtLaunchDeps()
		assert.Equal(t, d.mgmtLaunchDir(), deps.Dir, mode)
		assert.Nil(t, deps.Addresses, mode)
	}
	legacy := &Daemon{config: &config.Config{DataDir: t.TempDir(), Daemon: config.DaemonConfig{MgmtIPAM: MgmtIPAMLegacy}}}
	assert.Equal(t, vm.MgmtLaunchDeps{}, legacy.mgmtLaunchDeps())
}

// A durable node whose allocator never came up refuses a local start of a
// system VM before mounting its volumes, leaving its launch record unchanged.
func TestStart_DurableNodeWithoutAllocatorRefusesBeforeMount(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	d := &Daemon{
		config: &config.Config{DataDir: t.TempDir(), Daemon: config.DaemonConfig{MgmtIPAM: MgmtIPAMDurable}},
		node:   "node-a",
		vmMgr:  vm.NewManager(),
	}
	mounts := 0
	d.vmMgr.SetDeps(vm.Deps{
		NodeID:        d.node,
		VolumeMounter: &hookVolumeMounter{onMount: func(*vm.VM) { mounts++ }},
		MgmtLaunch:    d.mgmtLaunchDeps(),
	})
	instance := &vm.VM{ID: "i-sys", Status: vm.StateStopped, MgmtIP: "10.15.8.10"}
	d.vmMgr.Insert(instance)
	require.NoError(t, d.vmMgr.InitMgmtLaunch(instance))

	err := d.vmMgr.Start(t.Context(), "i-sys")

	require.ErrorContains(t, err, "management address record not wired")
	assert.Zero(t, mounts)
	rec := readLaunchRecord(t, d, "i-sys")
	assert.Equal(t, "claimed", rec["state"])
	assert.InDelta(t, 1, rec["generation"], 0)
}
