package vm

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppendMgmtNetcfgFwCfg(t *testing.T) {
	m := &Manager{}

	t.Run("emits a one-NIC netcfg blob and fw_cfg entry for the mgmt NIC", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		inst := &VM{ID: "i-mgmt01", MgmtMAC: "02:aa:bb:cc:dd:ee", MgmtIP: "10.20.0.5"}

		require.NoError(t, m.appendMgmtNetcfgFwCfg(inst))

		require.Len(t, inst.Config.FwCfg, 1)
		entry := inst.Config.FwCfg[0]
		require.Equal(t, "opt/spinifex/netcfg", entry.Name)

		data, err := os.ReadFile(entry.File)
		require.NoError(t, err)
		// Format and DEFAULT=0 must match build/microvm/init.sh + the eks-node
		// mulga-mgmt-net consumer; mgmt0 is never the default route.
		require.Equal(t, "NIC0_MAC=02:aa:bb:cc:dd:ee\nNIC0_CIDR=10.20.0.5/24\nNIC0_DEFAULT=0\n", string(data))
	})

	t.Run("no mgmt NIC is a no-op (customer guests have none)", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		inst := &VM{ID: "i-nomgmt"}

		require.NoError(t, m.appendMgmtNetcfgFwCfg(inst))
		require.Empty(t, inst.Config.FwCfg)
	})
}
