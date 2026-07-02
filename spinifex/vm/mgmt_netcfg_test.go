package vm

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppendSystemNetcfgFwCfg(t *testing.T) {
	m := &Manager{}

	t.Run("emits a data (DHCP) + mgmt (static) NIC blob for a multi-NIC system VM", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		inst := &VM{ID: "i-sys01", ENIMac: "02:3c:8f:54:bd:c9", MgmtMAC: "02:aa:bb:cc:dd:ee", MgmtIP: "10.20.0.5"}

		require.NoError(t, m.appendSystemNetcfgFwCfg(inst))

		require.Len(t, inst.Config.FwCfg, 1)
		entry := inst.Config.FwCfg[0]
		require.Equal(t, "opt/spinifex/netcfg", entry.Name)

		data, err := os.ReadFile(entry.File)
		require.NoError(t, err)
		// Format must match build/microvm/init.sh + the eks-node mulga-mgmt-net
		// consumer: the data ENI is DHCP + default route, mgmt0 is static and
		// never the default route.
		require.Equal(t,
			"NIC0_MAC=02:3c:8f:54:bd:c9\nNIC0_DHCP=1\nNIC0_DEFAULT=1\n"+
				"NIC1_MAC=02:aa:bb:cc:dd:ee\nNIC1_CIDR=10.20.0.5/24\nNIC1_DEFAULT=0\n",
			string(data))
	})

	t.Run("no mgmt NIC is a no-op (single-NIC guests use cloud-init/IMDS)", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		inst := &VM{ID: "i-nomgmt", ENIMac: "02:3c:8f:54:bd:c9"}

		require.NoError(t, m.appendSystemNetcfgFwCfg(inst))
		require.Empty(t, inst.Config.FwCfg)
	})
}
