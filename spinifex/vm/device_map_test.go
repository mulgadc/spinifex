package vm

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractPCIIndex(t *testing.T) {
	tests := []struct {
		name string
		qdev string
		want int
	}{
		{
			name: "standard peripheral-anon device",
			qdev: "/machine/peripheral-anon/device[0]/virtio-backend",
			want: 0,
		},
		{
			name: "device index 3",
			qdev: "/machine/peripheral-anon/device[3]/virtio-backend",
			want: 3,
		},
		{
			name: "hotplug device with higher index",
			qdev: "/machine/peripheral/hotplug-ebs1/device[12]/virtio-backend",
			want: 12,
		},
		{
			name: "unattached device path",
			qdev: "/machine/unattached/device[24]",
			want: 24,
		},
		{
			name: "empty string",
			qdev: "",
			want: -1,
		},
		{
			name: "no device brackets",
			qdev: "/machine/peripheral/something",
			want: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPCIIndex(tt.qdev)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestBuildDeviceMap covers the core device-mapping logic across boot devices,
// hot-plugged virtio peripherals, non-virtio devices that must be filtered out,
// PCI-index ordering across mixed boot/hot-plug input, and the empty case.
func TestBuildDeviceMap(t *testing.T) {
	tests := []struct {
		name    string
		devices []qmp.BlockDevice
		want    map[string]string // expected entries (subset checked with Equal)
		absent  []string          // keys that must NOT appear
		wantLen int
	}{
		{
			name:    "empty input",
			devices: nil,
			want:    map[string]string{},
			wantLen: 0,
		},
		{
			// Mirrors the example QMP query-block response from qmp.go:72
			name: "boot devices with non-virtio devices filtered out",
			devices: []qmp.BlockDevice{
				{
					IOStatus: "ok",
					Device:   "os",
					Inserted: &qmp.BlockInserted{
						Image: qmp.BlockImage{VirtualSize: 4294967296, Filename: "nbd://127.0.0.1:44801", Format: "raw"},
					},
					QDev: "/machine/peripheral-anon/device[0]/virtio-backend",
					Type: "unknown",
				},
				{
					IOStatus: "ok",
					Device:   "cloudinit",
					Inserted: &qmp.BlockInserted{
						Image: qmp.BlockImage{VirtualSize: 1048576, Filename: "nbd://127.0.0.1:42911", Format: "raw"},
						RO:    true,
					},
					QDev: "/machine/peripheral-anon/device[3]/virtio-backend",
					Type: "unknown",
				},
				{Device: "ide1-cd0", Removable: true, QDev: "/machine/unattached/device[24]"},
				{Device: "floppy0", Removable: true, QDev: "/machine/unattached/device[18]"},
				{Device: "sd0", Removable: true},
			},
			want:    map[string]string{"os": "/dev/vda", "cloudinit": "/dev/vdb"},
			absent:  []string{"ide1-cd0", "floppy0", "sd0"},
			wantLen: 2,
		},
		{
			// Hot-plugged devices have empty Device field and use the
			// /machine/peripheral/<id>/virtio-backend QDev path format.
			name: "boot plus hot-plugged devices",
			devices: []qmp.BlockDevice{
				{Device: "os", Inserted: &qmp.BlockInserted{}, QDev: "/machine/peripheral-anon/device[0]/virtio-backend"},
				{Device: "cloudinit", Inserted: &qmp.BlockInserted{}, QDev: "/machine/peripheral-anon/device[3]/virtio-backend"},
				{Device: "", Inserted: &qmp.BlockInserted{}, QDev: "/machine/peripheral/vdisk-vol-abc123/virtio-backend"},
				{Device: "", Inserted: &qmp.BlockInserted{}, QDev: "/machine/peripheral/vdisk-vol-def456/virtio-backend"},
				{Device: "floppy0", QDev: "/machine/unattached/device[18]"},
				{Device: "sd0"},
				{Device: "ide1-cd0", QDev: "/machine/unattached/device[24]"},
			},
			want: map[string]string{
				"os":               "/dev/vda",
				"cloudinit":        "/dev/vdb",
				"vdisk-vol-abc123": "/dev/vdc",
				"vdisk-vol-def456": "/dev/vdd",
			},
			wantLen: 4,
		},
		{
			// Boot devices returned out of order: lowest PCI index wins /dev/vda;
			// hot-plugged devices sort after boot devices.
			name: "PCI-index ordering across mixed boot and hot-plug",
			devices: []qmp.BlockDevice{
				{Device: "cloudinit", Inserted: &qmp.BlockInserted{}, QDev: "/machine/peripheral-anon/device[5]/virtio-backend"},
				{Device: "os", Inserted: &qmp.BlockInserted{}, QDev: "/machine/peripheral-anon/device[1]/virtio-backend"},
				{Device: "", Inserted: &qmp.BlockInserted{}, QDev: "/machine/peripheral/vdisk-vol-123/virtio-backend"},
			},
			want: map[string]string{
				"os":            "/dev/vda",
				"cloudinit":     "/dev/vdb",
				"vdisk-vol-123": "/dev/vdc",
			},
			wantLen: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildDeviceMap(tt.devices)
			for k, v := range tt.want {
				assert.Equal(t, v, got[k], "device %q", k)
			}
			for _, k := range tt.absent {
				assert.NotContains(t, got, k)
			}
			assert.Len(t, got, tt.wantLen)
		})
	}
}

func TestExtractPeripheralName(t *testing.T) {
	tests := []struct {
		name string
		qdev string
		want string
	}{
		{
			name: "hot-plugged volume",
			qdev: "/machine/peripheral/vdisk-vol-abc123/virtio-backend",
			want: "vdisk-vol-abc123",
		},
		{
			name: "boot device path",
			qdev: "/machine/peripheral-anon/device[0]/virtio-backend",
			want: "",
		},
		{
			name: "empty",
			qdev: "",
			want: "",
		},
		{
			name: "no trailing slash",
			qdev: "/machine/peripheral/",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractPeripheralName(tt.qdev))
		})
	}
}

// stubDeviceMapRetrySleep replaces the package-level sleep seam with a
// recorder so retry tests can assert sleep cadence without wall-clock time.
func stubDeviceMapRetrySleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var sleeps []time.Duration
	prev := deviceMapRetrySleep
	deviceMapRetrySleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	t.Cleanup(func() { deviceMapRetrySleep = prev })
	return &sleeps
}

func TestQueryGuestDeviceMapWait(t *testing.T) {
	const retryDelay = 200 * time.Millisecond

	bootDevice := func(id, qdev string) qmp.BlockDevice {
		return qmp.BlockDevice{
			IOStatus: "ok",
			Device:   id,
			Inserted: &qmp.BlockInserted{},
			QDev:     qdev,
			Type:     "unknown",
		}
	}

	t.Run("device present on first attempt makes no extra calls", func(t *testing.T) {
		sleeps := stubDeviceMapRetrySleep(t)
		var calls atomic.Int32
		qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
			calls.Add(1)
			require.Equal(t, "query-block", cmd.Execute)
			return map[string]any{
				"return": []qmp.BlockDevice{
					bootDevice("os", "/machine/peripheral-anon/device[0]/virtio-backend"),
					bootDevice("vdisk-vol-new", "/machine/peripheral/vdisk-vol-new/virtio-backend"),
				},
			}
		})
		defer cancel()

		deviceMap, err := queryGuestDeviceMapWait(qmpClient, "i-1", "vdisk-vol-new")

		require.NoError(t, err)
		assert.Contains(t, deviceMap, "vdisk-vol-new")
		assert.Equal(t, int32(1), calls.Load(), "exactly one query-block call expected")
		assert.Empty(t, *sleeps, "no sleep should occur on first-attempt success")
	})

	t.Run("device missing all 5 attempts invokes final probe and returns last result", func(t *testing.T) {
		sleeps := stubDeviceMapRetrySleep(t)
		var calls atomic.Int32
		qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
			calls.Add(1)
			require.Equal(t, "query-block", cmd.Execute)
			return map[string]any{
				"return": []qmp.BlockDevice{
					bootDevice("os", "/machine/peripheral-anon/device[0]/virtio-backend"),
				},
			}
		})
		defer cancel()

		deviceMap, err := queryGuestDeviceMapWait(qmpClient, "i-1", "vdisk-vol-missing")

		require.NoError(t, err)
		assert.Equal(t, int32(6), calls.Load(),
			"5 retry attempts plus the final fall-through probe must all run")
		assert.Equal(t, []time.Duration{retryDelay, retryDelay, retryDelay, retryDelay}, *sleeps,
			"4 sleeps must occur between the 5 in-loop attempts (none after the final fall-through)")
		assert.NotContains(t, deviceMap, "vdisk-vol-missing",
			"missing device must be absent from the returned map so callers can detect the failure")
		assert.Contains(t, deviceMap, "os")
	})

	t.Run("device appears on third attempt skips remaining retries", func(t *testing.T) {
		sleeps := stubDeviceMapRetrySleep(t)
		var calls atomic.Int32
		qmpClient, cancel := newMockQMPClient(t, func(cmd qmp.QMPCommand) map[string]any {
			n := calls.Add(1)
			require.Equal(t, "query-block", cmd.Execute)
			devices := []qmp.BlockDevice{
				bootDevice("os", "/machine/peripheral-anon/device[0]/virtio-backend"),
			}
			if n >= 3 {
				devices = append(devices, bootDevice("vdisk-vol-late", "/machine/peripheral/vdisk-vol-late/virtio-backend"))
			}
			return map[string]any{"return": devices}
		})
		defer cancel()

		deviceMap, err := queryGuestDeviceMapWait(qmpClient, "i-1", "vdisk-vol-late")

		require.NoError(t, err)
		assert.Contains(t, deviceMap, "vdisk-vol-late")
		assert.Equal(t, int32(3), calls.Load(),
			"third-attempt success must not trigger a 4th or 5th probe")
		assert.Equal(t, []time.Duration{retryDelay, retryDelay}, *sleeps,
			"exactly 2 sleeps must occur (after attempts 1 and 2); none after the 3rd success")
	})
}

func TestExtractHotplugPort(t *testing.T) {
	tests := []struct {
		name string
		qdev string
		want int
	}{
		{
			name: "hotplug-ebs port 3",
			qdev: "/machine/peripheral/vdisk-vol-xxx/hotplug-ebs3/virtio-backend",
			want: 3,
		},
		{
			name: "no hotplug in path",
			qdev: "/machine/peripheral/vdisk-vol-xxx/virtio-backend",
			want: -1,
		},
		{
			name: "boot device",
			qdev: "/machine/peripheral-anon/device[0]/virtio-backend",
			want: -1,
		},
		{
			name: "hotplug-eni port ignored (not a block device port)",
			qdev: "/machine/peripheral/net-eni-1/hotplug-eni1/virtio-backend",
			want: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractHotplugPort(tt.qdev))
		})
	}
}

func TestFreeHotplugEBSPort(t *testing.T) {
	mk := func(ports ...int) []types.EBSRequest {
		reqs := make([]types.EBSRequest, 0, len(ports))
		for _, p := range ports {
			reqs = append(reqs, types.EBSRequest{HotplugPort: p})
		}
		return reqs
	}

	tests := []struct {
		name string
		reqs []types.EBSRequest
		want int
	}{
		{name: "empty pool returns 1", reqs: nil, want: 1},
		{name: "first port taken returns 2", reqs: mk(1), want: 2},
		{name: "lowest gap is reused", reqs: mk(1, 3), want: 2},
		{name: "boot/zero ports ignored", reqs: mk(0, 0), want: 1},
		{name: "contiguous run returns next", reqs: mk(1, 2, 3), want: 4},
		{name: "out-of-order still finds gap", reqs: mk(3, 1), want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, freeHotplugEBSPort(tt.reqs))
		})
	}

	t.Run("exhausted pool returns 0", func(t *testing.T) {
		full := make([]types.EBSRequest, 0, EBSHotPlugSlotCount)
		for p := 1; p <= EBSHotPlugSlotCount; p++ {
			full = append(full, types.EBSRequest{HotplugPort: p})
		}
		assert.Equal(t, 0, freeHotplugEBSPort(full))
	})
}
