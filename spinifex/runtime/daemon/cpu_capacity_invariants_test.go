package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestS1_HostVCPUIsOnlineCPUs enforces ADR-0015:S1: one instance vCPU is one
// CPU the kernel has online, a thread with SMT on and a core with SMT off or on
// ARM, as AWS counts a vCPU. The list is what /sys/devices/system/cpu/online holds.
func TestS1_HostVCPUIsOnlineCPUs(t *testing.T) {
	cases := []struct {
		host   string
		online string
		want   int
	}{
		{"8 cores with SMT on", "0-15", 16},
		{"8 cores with SMT off at boot (nosmt)", "0-7", 8},
		{"8 cores with SMT siblings offlined at runtime", "0-3,8-11", 8},
		{"ARM, 64 cores, no SMT", "0-63", 64},
		{"single CPU", "0", 1},
		{"two sockets with a CPU offline", "0-30,32-63", 63},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			got, ok := parseCPUList(tc.online + "\n")
			require.Truef(t, ok, "ADR-0015:S1: could not read the online CPU list %q", tc.online)
			assert.Equalf(t, tc.want, got,
				"ADR-0015:S1: %s lists %q online, so the host has %d vCPU", tc.host, tc.online, tc.want)
		})
	}

	for _, bad := range []string{"", "x", "3-1", "-1"} {
		_, ok := parseCPUList(bad)
		assert.Falsef(t, ok, "ADR-0015:S1: %q is not a CPU list and must fall back to runtime.NumCPU", bad)
	}
}

// TestS2_ReserveTiers enforces ADR-0015:S2 at every tier boundary: the CPUs
// held back for the OS and Spinifex, and so the CPUs instances may use.
func TestS2_ReserveTiers(t *testing.T) {
	cases := []struct{ host, reserve int }{
		{2, 1}, {3, 1}, {4, 1}, {8, 1},
		{9, 2}, {16, 2},
		{17, 4}, {31, 4}, {32, 4}, {63, 4},
		{64, 8}, {96, 8}, {128, 8}, {256, 8},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d_cpus", tc.host), func(t *testing.T) {
			assert.Equalf(t, tc.reserve, reservedVCPUFor(tc.host),
				"ADR-0015:S2: a host with %d online CPUs reserves %d", tc.host, tc.reserve)

			vcpu, _, err := applyHostReserve(resolveHostReserve(func(string) string { return "" }, tc.host), tc.host, 64)
			require.NoErrorf(t, err, "ADR-0015:S2: a host with %d online CPUs must start", tc.host)
			assert.Equalf(t, tc.host-tc.reserve, tc.host-vcpu,
				"ADR-0015:S2: a host with %d online CPUs schedules %d", tc.host, tc.host-tc.reserve)
		})
	}

	_, _, err := applyHostReserve(resolveHostReserve(func(string) string { return "" }, 1), 1, 64)
	assert.Error(t, err, "ADR-0015:S2: a 1-CPU host reserves its only CPU and must refuse to start rather than schedule onto it")
}

// TestS3_OneInstanceVCPUPerSchedulableCPU enforces ADR-0015:S3: there is no CPU
// overcommit, so instance vCPUs never exceed the CPUs left after the reserve.
func TestS3_OneInstanceVCPUPerSchedulableCPU(t *testing.T) {
	const host = 16
	schedulable := host - reservedVCPUFor(host)

	assert.Equal(t, schedulable/2, canAllocateCount(schedulable, 0, 60, 0, 2, 2048, 100, 0, false),
		"ADR-0015:S3: a 16-CPU host fits 7 two-vCPU instances and no more")
	assert.Equal(t, 0, canAllocateCount(schedulable, schedulable, 60, 0, 1, 512, 100, 0, false),
		"ADR-0015:S3: with every schedulable CPU allocated, no instance may be admitted")
}

// TestS5_OverridesAreOperatorChoices enforces ADR-0015:S5: SPINIFEX_HOST_VCPU and
// SPINIFEX_RESERVED_VCPU replace the detected values only when an operator sets them.
func TestS5_OverridesAreOperatorChoices(t *testing.T) {
	none := func(string) string { return "" }
	assert.Equal(t, 16, resolveHostVCPU(none, 16), "ADR-0015:S5: with no override the online CPU count is used")
	assert.Equal(t, reservedVCPUFor(16), resolveHostReserve(none, 16).vCPU, "ADR-0015:S5: with no override the S2 tier is used")

	set := map[string]string{"SPINIFEX_HOST_VCPU": "12", "SPINIFEX_RESERVED_VCPU": "3"}
	get := func(k string) string { return set[k] }
	assert.Equal(t, 12, resolveHostVCPU(get, 16), "ADR-0015:S5: SPINIFEX_HOST_VCPU replaces the detected count")
	assert.Equal(t, 3, resolveHostReserve(get, 16).vCPU, "ADR-0015:S5: SPINIFEX_RESERVED_VCPU replaces the tier")
}
