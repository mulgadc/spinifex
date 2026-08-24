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

	// The guest configures by MAC, so a NIC the blob omits is left down and
	// address-less. An RDS DB VM spans three: the system-VPC ENI, the customer
	// ENI it is actually reached on, and mgmt0. The customer ENI is static
	// (NIC1_CIDR + NIC1_GW), not DHCP: OVN's lease is never renewed by the
	// guest's one-shot udhcpc, so a DHCP customer ENI goes address-less an
	// hour after boot.
	t.Run("names every NIC the VM is given, customer ENI static and never the default route", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		inst := &VM{
			ID:      "i-rds01",
			ENIMac:  "02:3c:8f:54:bd:c9",
			MgmtMAC: "02:aa:bb:cc:dd:ee",
			MgmtIP:  "10.20.0.5",
			ExtraENIs: []ExtraENI{
				{
					ENIID:         "eni-customer",
					ENIMac:        "02:31:ed:a2:ed:27",
					ENIIP:         "172.31.0.4",
					ENICIDRPrefix: 20,
					Gateway:       "172.31.0.1",
				},
			},
		}

		require.NoError(t, m.appendSystemNetcfgFwCfg(inst))

		require.Len(t, inst.Config.FwCfg, 1)
		data, err := os.ReadFile(inst.Config.FwCfg[0].File)
		require.NoError(t, err)
		blob := string(data)
		require.Equal(t,
			"NIC0_MAC=02:3c:8f:54:bd:c9\nNIC0_DHCP=1\nNIC0_DEFAULT=1\n"+
				"NIC1_MAC=02:31:ed:a2:ed:27\nNIC1_CIDR=172.31.0.4/20\nNIC1_GW=172.31.0.1\nNIC1_DEFAULT=0\n"+
				"NIC2_MAC=02:aa:bb:cc:dd:ee\nNIC2_CIDR=10.20.0.5/24\nNIC2_DEFAULT=0\n",
			blob)
		require.NotContains(t, blob, "NIC1_DHCP", "the customer ENI must not depend on the OVN DHCP lease being renewed")
	})

	// An extra ENI without a resolved prefix/gateway (e.g. persisted state from
	// before this field existed) falls back to DHCP rather than emitting a
	// bogus static config.
	t.Run("extra ENI without a resolved gateway falls back to DHCP", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		inst := &VM{
			ID:      "i-rds02",
			ENIMac:  "02:3c:8f:54:bd:c9",
			MgmtMAC: "02:aa:bb:cc:dd:ee",
			MgmtIP:  "10.20.0.5",
			ExtraENIs: []ExtraENI{
				{ENIID: "eni-customer", ENIMac: "02:31:ed:a2:ed:27"},
			},
		}

		require.NoError(t, m.appendSystemNetcfgFwCfg(inst))

		require.Len(t, inst.Config.FwCfg, 1)
		data, err := os.ReadFile(inst.Config.FwCfg[0].File)
		require.NoError(t, err)
		require.Equal(t,
			"NIC0_MAC=02:3c:8f:54:bd:c9\nNIC0_DHCP=1\nNIC0_DEFAULT=1\n"+
				"NIC1_MAC=02:31:ed:a2:ed:27\nNIC1_DHCP=1\nNIC1_DEFAULT=0\n"+
				"NIC2_MAC=02:aa:bb:cc:dd:ee\nNIC2_CIDR=10.20.0.5/24\nNIC2_DEFAULT=0\n",
			string(data))
	})

	t.Run("no mgmt NIC is a no-op (single-NIC guests use cloud-init/IMDS)", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		inst := &VM{ID: "i-nomgmt", ENIMac: "02:3c:8f:54:bd:c9"}

		require.NoError(t, m.appendSystemNetcfgFwCfg(inst))
		require.Empty(t, inst.Config.FwCfg)
	})
}
