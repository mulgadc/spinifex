package daemon

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// launchedSQSStore launches a store through the normal path and returns the
// running VM, the fw_cfg contents it booted with, and the volume ID. Every
// mount the fake mounter sees is appended to mounts.
func launchedSQSStore(t *testing.T, mounts *[][]vm.EBSRequest) (*sysInstFixture, *vm.VM, map[string]string, string) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	f := newSysInstFixture(t)
	f.sqsStoreVolumes(t)
	sqsStoreImage(t)
	vol := f.systemVolume(t)

	var launched *vm.VM
	f.d.vmMgr.WaitForBackgroundWork()
	f.d.vmMgr.SetDeps(vm.Deps{
		NodeID: f.d.node,
		VolumeMounter: &hookVolumeMounter{onMount: func(v *vm.VM) {
			launched = v
			v.EBSRequests.Mu.Lock()
			*mounts = append(*mounts, append([]vm.EBSRequest(nil), v.EBSRequests.Requests...))
			v.EBSRequests.Mu.Unlock()
			f.d.vmMgr.UpdateState(v.ID, func(x *vm.VM) { x.Status = vm.StateRunning })
		}},
		VolumeStateUpdater: f.d.volumeService,
		Hooks:              vm.ManagerHooks{BeforeInstanceRelaunch: f.d.prepareInstanceRelaunch},
	})
	_, err := f.d.LaunchSQSStoreInstance(f.sqsInput(t, f.newENI(t), vol))
	require.NoError(t, err)
	require.NotNil(t, launched)
	return f, launched, fwCfgContents(t, launched), vol
}

func fwCfgContents(t *testing.T, v *vm.VM) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, e := range v.Config.FwCfg {
		b, err := os.ReadFile(e.File)
		require.NoError(t, err, "fw_cfg file %s", e.Name)
		got[e.Name] = string(b)
	}
	return got
}

// persisted round-trips the VM through its JSON record, as a daemon restart
// reads it back, and removes the tmpfs fw_cfg files a host reboot wipes.
func persisted(t *testing.T, v *vm.VM) *vm.VM {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var back vm.VM
	require.NoError(t, json.Unmarshal(b, &back))
	for _, e := range back.Config.FwCfg {
		require.NoError(t, os.Remove(e.File))
	}
	return &back
}

func TestSQSStoreRelaunch_RebuildsLostGuestConfigAndRemountsTheVolume(t *testing.T) {
	var mounts [][]vm.EBSRequest
	f, launched, booted, vol := launchedSQSStore(t, &mounts)

	g := launched.SQSStoreGuest
	require.NotNil(t, g, "the launch must record the guest config")
	assert.Equal(t, f.d.node, g.NodeID)
	assert.True(t, nkeys.IsValidPublicUserKey(g.ClientNKey), "only the public key is recorded")

	restored := persisted(t, launched)
	f.d.vmMgr.Insert(restored)
	restored.Status = vm.StatePending
	require.NoError(t, f.d.vmMgr.PrepareRelaunch(restored))
	assert.Equal(t, booted, fwCfgContents(t, restored), "the relaunch must boot exactly the config the launch did")

	require.NoError(t, f.d.vmMgr.Run(t.Context(), restored))
	require.Len(t, mounts, 2, "the relaunch mounts through the normal EBS path")
	assert.Equal(t, []vm.EBSRequest{{Name: vol, DeviceName: sqsStoreDataDevice, DeleteOnTermination: false}}, mounts[1])
}

func TestSQSStoreRelaunch_RefusesWithoutEverythingItNeeds(t *testing.T) {
	seed := func(t *testing.T) string {
		kp, err := nkeys.CreateUser()
		require.NoError(t, err)
		s, err := kp.Seed()
		require.NoError(t, err)
		return string(s)
	}
	cases := []struct {
		name   string
		mutate func(t *testing.T, v *vm.VM)
		want   string
	}{
		{"no guest config", func(_ *testing.T, v *vm.VM) { v.SQSStoreGuest = nil }, "no guest config"},
		{"launched on another node", func(_ *testing.T, v *vm.VM) { v.SQSStoreGuest.NodeID = "node-elsewhere" }, "only restarted there"},
		{"no recorded node", func(_ *testing.T, v *vm.VM) { v.SQSStoreGuest.NodeID = "" }, "only restarted there"},
		{"malformed volume", func(_ *testing.T, v *vm.VM) { v.SQSStoreGuest.DataVolumeID = "vol-123" }, "malformed"},
		{"seed in place of the key", func(t *testing.T, v *vm.VM) { v.SQSStoreGuest.ClientNKey = seed(t) }, "not a NATS user public key"},
		{"no listen address", func(_ *testing.T, v *vm.VM) { v.SQSStoreGuest.ListenAddr = "" }, "listen address"},
		{"loopback listen address", func(_ *testing.T, v *vm.VM) { v.SQSStoreGuest.ListenAddr = "127.0.0.1" }, "listen address"},
		{"no network config", func(_ *testing.T, v *vm.VM) { v.SQSStoreGuest.Netcfg = "" }, "no network config"},
		{"launched before gateway authentication", func(_ *testing.T, v *vm.VM) {
			v.SQSStoreGuest.Region, v.SQSStoreGuest.GatewayURL, v.SQSStoreGuest.CACert = "", "", ""
		}, "malformed"},
		{"no gateway URL or CA", func(_ *testing.T, v *vm.VM) { v.SQSStoreGuest.GatewayURL, v.SQSStoreGuest.CACert = "", "" },
			"lacks the gateway URL or CA"},
		{"data volume not recorded", func(_ *testing.T, v *vm.VM) { v.EBSRequests.Requests = nil }, "not among its kept volumes"},
		{"data volume deleted on terminate", func(_ *testing.T, v *vm.VM) { v.EBSRequests.Requests[0].DeleteOnTermination = true }, "not among its kept volumes"},
		{"fw_cfg files moved", func(_ *testing.T, v *vm.VM) { v.Config.FwCfg[1].File += ".other" }, "not the store's"},
		{"fw_cfg file missing", func(_ *testing.T, v *vm.VM) { v.Config.FwCfg = v.Config.FwCfg[:3] }, "not the store's"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mounts [][]vm.EBSRequest
			f, launched, _, _ := launchedSQSStore(t, &mounts)
			restored := persisted(t, launched)
			files := restored.Config.FwCfg
			tc.mutate(t, restored)

			err := f.d.prepareInstanceRelaunch(restored)
			require.ErrorContains(t, err, tc.want)
			assert.NotContains(t, err.Error(), "SU", "a seed must never be echoed")
			for _, e := range files {
				_, statErr := os.Stat(e.File)
				assert.True(t, os.IsNotExist(statErr), "a refused relaunch writes no fw_cfg file: %s", e.Name)
			}
		})
	}
}
