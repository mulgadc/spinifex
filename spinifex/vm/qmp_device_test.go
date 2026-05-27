package vm

import (
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStubDeviceController_AddDelRoundTrip(t *testing.T) {
	s := NewStubDeviceController()

	require.NoError(t, s.NetdevAdd(map[string]any{"id": "hostnet-eni-1", "type": "tap"}))
	require.True(t, s.HasNetdev("hostnet-eni-1"))

	require.NoError(t, s.DeviceAdd(map[string]any{"id": "net-eni-1", "driver": "virtio-net-pci"}))
	require.True(t, s.HasDevice("net-eni-1"))

	pci, err := s.QueryPCI()
	require.NoError(t, err)
	require.Len(t, pci, 1)
	assert.Equal(t, "net-eni-1", pci[0].QDevID)

	require.NoError(t, s.DeviceDel("net-eni-1"))
	require.False(t, s.HasDevice("net-eni-1"))

	require.NoError(t, s.NetdevDel("hostnet-eni-1"))
	require.False(t, s.HasNetdev("hostnet-eni-1"))

	executes := []string{}
	for _, c := range s.Calls() {
		executes = append(executes, c.Execute)
	}
	assert.Equal(t, []string{
		"netdev_add", "device_add", "query-pci", "device_del", "netdev_del",
	}, executes)
}

func TestStubDeviceController_DuplicateAddRejected(t *testing.T) {
	s := NewStubDeviceController()
	require.NoError(t, s.DeviceAdd(map[string]any{"id": "net-eni-1"}))

	err := s.DeviceAdd(map[string]any{"id": "net-eni-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate id")

	require.NoError(t, s.NetdevAdd(map[string]any{"id": "hostnet-eni-1"}))
	err = s.NetdevAdd(map[string]any{"id": "hostnet-eni-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate id")
}

func TestStubDeviceController_MissingIDRejected(t *testing.T) {
	s := NewStubDeviceController()
	require.Error(t, s.DeviceAdd(map[string]any{"driver": "virtio-net-pci"}))
	require.Error(t, s.NetdevAdd(map[string]any{"type": "tap"}))
}

func TestStubDeviceController_DelUnknownRejected(t *testing.T) {
	s := NewStubDeviceController()
	require.Error(t, s.DeviceDel("net-eni-missing"))
	require.Error(t, s.NetdevDel("hostnet-eni-missing"))
}

func TestStubDeviceController_FailNextIsOneShot(t *testing.T) {
	s := NewStubDeviceController()
	injected := errors.New("simulated wire failure")
	s.SetFailNext("device_add", injected)

	err := s.DeviceAdd(map[string]any{"id": "net-eni-1"})
	require.ErrorIs(t, err, injected)
	assert.False(t, s.HasDevice("net-eni-1"), "failed DeviceAdd must not persist state")

	// One-shot: second call succeeds.
	require.NoError(t, s.DeviceAdd(map[string]any{"id": "net-eni-1"}))
	assert.True(t, s.HasDevice("net-eni-1"))
}

func TestStubDeviceController_CallsSnapshotIsCopy(t *testing.T) {
	s := NewStubDeviceController()
	require.NoError(t, s.DeviceAdd(map[string]any{"id": "net-eni-1"}))

	snap := s.Calls()
	snap[0].Execute = "mutated"

	again := s.Calls()
	require.Len(t, again, 1)
	assert.Equal(t, "device_add", again[0].Execute, "Calls() must return independent copies")
}

func TestQMPDeviceController_QueryPCIFlattensBridges(t *testing.T) {
	// query-pci returns one bus whose root devices include pcie-root-ports
	// (each itself a PCI-PCI bridge). The hot-plugged virtio-net device
	// sits under the bridge. The parser must recurse and flatten.
	client, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		require.Equal(t, "query-pci", cmd.Execute)
		return map[string]any{
			"return": []any{
				map[string]any{
					"bus": 0,
					"devices": []any{
						map[string]any{"bus": 0, "slot": 1, "qdev_id": "primary"},
						map[string]any{
							"bus": 0, "slot": 2, "qdev_id": "hotplug-eni1",
							"pci_bridge": map[string]any{
								"devices": []any{
									map[string]any{"bus": 5, "slot": 0, "qdev_id": "net-eni-1"},
								},
							},
						},
					},
				},
			},
		}
	})
	defer cancel()

	c := NewQMPDeviceController(client, "i-test")
	pci, err := c.QueryPCI()
	require.NoError(t, err)
	ids := make([]string, 0, len(pci))
	for _, d := range pci {
		ids = append(ids, d.QDevID)
	}
	assert.ElementsMatch(t, []string{"primary", "hotplug-eni1", "net-eni-1"}, ids)
}

func TestQMPDeviceController_DispatchesExpectedCommands(t *testing.T) {
	rec := &qmpRecorder{}
	client, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
		rec.record(cmd)
		return map[string]any{"return": map[string]any{}}
	})
	defer cancel()

	c := NewQMPDeviceController(client, "i-test")

	require.NoError(t, c.NetdevAdd(map[string]any{"id": "hostnet-eni-1", "type": "tap"}))
	require.NoError(t, c.DeviceAdd(map[string]any{"id": "net-eni-1", "driver": "virtio-net-pci"}))
	require.NoError(t, c.DeviceDel("net-eni-1"))
	require.NoError(t, c.NetdevDel("hostnet-eni-1"))

	assert.Equal(t, []string{"netdev_add", "device_add", "device_del", "netdev_del"}, rec.executes())
}
