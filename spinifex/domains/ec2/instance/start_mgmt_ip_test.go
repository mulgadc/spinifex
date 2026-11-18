//test:in-package — it swaps the manager's dependencies, which the fixture owns.
package instance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	startMgmtNode = "node-a"
	startMgmtIP   = "10.15.8.10"
)

// heldMgmtAddresses is the durable reservation record: Claim takes a free
// address, Confirm only re-acknowledges one the instance already holds.
type heldMgmtAddresses struct {
	mu       sync.Mutex
	held     map[string]string
	claims   int
	confirms int
}

var _ vm.MgmtAddresses = (*heldMgmtAddresses)(nil)

func (h *heldMgmtAddresses) Claim(id, ip string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.claims++
	if owner, ok := h.held[ip]; ok && owner != id {
		return errors.New("address held by another instance")
	}
	h.held[ip] = id
	return nil
}

func (h *heldMgmtAddresses) Confirm(id, ip string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.confirms++
	if h.held[ip] != id {
		return errors.New("no reservation to confirm")
	}
	return nil
}

func (h *heldMgmtAddresses) Release(id, ip string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held[ip] == id {
		delete(h.held, ip)
	}
	return nil
}

type acceptingVolumeState struct{}

func (acceptingVolumeState) UpdateVolumeState(_, _, _, _, _ string) error { return nil }

// stoppedMgmtStart is a system VM an operator stop has left behind: its
// address released and its last launch recorded as exited.
func stoppedMgmtStart(t *testing.T, held map[string]string) (*startFixture, *heldMgmtAddresses, string) {
	t.Helper()
	id := "i-mgmt-start"
	stopped := stoppedWithVolumes("acc", id)
	stopped.InstanceType = "t3.micro"
	stopped.MgmtIP = startMgmtIP
	f := newStartFixture(t, stopped)
	addrs := &heldMgmtAddresses{held: held}
	dir := t.TempDir()
	rec, err := json.Marshal(map[string]any{
		"schema": 1, "instance_id": id, "node": startMgmtNode, "mgmt_ip": startMgmtIP,
		"boot_id": "boot-before", "generation": 2, "state": "exited",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, id+".json"), rec, 0o600))
	f.svc.vmMgr.SetDeps(vm.Deps{
		NodeID:             startMgmtNode,
		VolumeMounter:      raceVolumeMounter{},
		VolumeStateUpdater: acceptingVolumeState{},
		MgmtLaunch:         vm.MgmtLaunchDeps{Dir: dir, Addresses: addrs},
	})
	return f, addrs, filepath.Join(dir, id+".json")
}

func launchGeneration(t *testing.T, path string) float64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var rec map[string]any
	require.NoError(t, json.Unmarshal(raw, &rec))
	gen, ok := rec["generation"].(float64)
	require.True(t, ok)
	return gen
}

// A customer start after an operator stop takes its released address back.
// The launch cannot finish without a hypervisor; the claim and the new
// generation are recorded before QEMU would start.
func TestStartStoppedInstance_TakesBackItsReleasedManagementAddress(t *testing.T) {
	f, addrs, path := stoppedMgmtStart(t, map[string]string{})

	_ = f.start(t, "i-mgmt-start")

	assert.Equal(t, 1, addrs.claims)
	assert.Equal(t, "i-mgmt-start", addrs.held[startMgmtIP])
	assert.InDelta(t, 3, launchGeneration(t, path), 0)
}

func TestStartStoppedInstance_RefusesAnAddressAnotherInstanceHolds(t *testing.T) {
	f, addrs, path := stoppedMgmtStart(t, map[string]string{startMgmtIP: "i-other"})

	require.Error(t, f.start(t, "i-mgmt-start"))

	assert.Equal(t, "i-other", addrs.held[startMgmtIP])
	assert.InDelta(t, 2, launchGeneration(t, path), 0)
}

// Recovery from a node that stopped heartbeating only re-confirms a
// reservation; it never re-creates one that was released.
func TestRecoverInstance_DoesNotRecreateAReleasedManagementAddress(t *testing.T) {
	f, addrs, path := stoppedMgmtStart(t, map[string]string{})
	instance := f.store.Stopped["i-mgmt-start"]

	require.Error(t, f.svc.RecoverInstance(context.Background(), instance, func() {}))

	assert.Zero(t, addrs.claims)
	assert.Equal(t, 1, addrs.confirms, "the recovery reached the address check")
	assert.Empty(t, addrs.held)
	assert.InDelta(t, 2, launchGeneration(t, path), 0)
}
