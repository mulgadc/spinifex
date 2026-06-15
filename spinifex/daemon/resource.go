package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/types"
)

// errInsufficientCapacity is returned by allocateForLaunch when MinCount
// cannot be satisfied.
var errInsufficientCapacity = errors.New("insufficient capacity to satisfy MinCount")

// hostReserve holds CPU and RAM reserved for the daemon and co-located fixed
// services. Per-instance costs (nbdkit) are charged separately. Tunable via
// SPINIFEX_RESERVED_VCPU / SPINIFEX_RESERVED_MEM_GB.
type hostReserve struct {
	vCPU  int
	memGB float64
}

// defaultHostReserve is the conservative floor for the fixed infrastructure tier.
var defaultHostReserve = hostReserve{vCPU: 2, memGB: 2.0}

// resolveHostReserve returns defaultHostReserve overridden by
// SPINIFEX_RESERVED_VCPU / SPINIFEX_RESERVED_MEM_GB when valid.
func resolveHostReserve(getenv func(string) string) hostReserve {
	r := defaultHostReserve
	if v := getenv("SPINIFEX_RESERVED_VCPU"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			r.vCPU = n
		} else {
			slog.Warn("ignoring SPINIFEX_RESERVED_VCPU", "value", v, "err", err)
		}
	}
	if v := getenv("SPINIFEX_RESERVED_MEM_GB"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			r.memGB = f
		} else {
			slog.Warn("ignoring SPINIFEX_RESERVED_MEM_GB", "value", v, "err", err)
		}
	}
	return r
}

// resolveHostVCPU returns detected core count, overridden by SPINIFEX_HOST_VCPU
// when valid. Override is an escape hatch for hosts with misreported topology.
func resolveHostVCPU(getenv func(string) string, detected int) int {
	if v := getenv("SPINIFEX_HOST_VCPU"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		slog.Warn("ignoring SPINIFEX_HOST_VCPU", "value", v)
	}
	return detected
}

// minHostMemHeadroomGB is the minimum schedulable memory above the reserve.
const minHostMemHeadroomGB = 0.5

// applyHostReserve validates host capacity against the reserve and returns the
// reserve to apply. Checks CPU and memory separately for clear error messages.
func applyHostReserve(host hostReserve, totalVCPU int, totalMemGB float64) (vcpu int, mem float64, err error) {
	if totalVCPU <= host.vCPU {
		return 0, 0, fmt.Errorf(
			"host vCPU below required minimum: have %d, need at least %d (reserve %d + 1 schedulable)",
			totalVCPU, host.vCPU+1, host.vCPU,
		)
	}
	if totalMemGB < host.memGB+minHostMemHeadroomGB {
		return 0, 0, fmt.Errorf(
			"host memory below required minimum: have %.2f GB, need at least %.2f GB (reserve %.1f + %.1f headroom)",
			totalMemGB, host.memGB+minHostMemHeadroomGB, host.memGB, minHostMemHeadroomGB,
		)
	}
	return host.vCPU, host.memGB, nil
}

// canAllocateCount returns how many instances of the given type fit in remaining
// capacity, capped at maxCount. Logs an error if capacity is negative.
func canAllocateCount(availVCPU, allocVCPU int, availMem, allocMem float64,
	vCPUs int64, memMiB int64, maxCount int,
	availGPU int, requiresGPU bool) int {
	if requiresGPU && availGPU == 0 {
		return 0
	}

	remainingVCPU := availVCPU - allocVCPU
	remainingMem := availMem - allocMem
	if remainingVCPU < 0 || remainingMem < 0 {
		slog.Error("schedulable capacity negative — reserve misconfigured or allocation drift",
			"availVCPU", availVCPU, "allocVCPU", allocVCPU, "remainingVCPU", remainingVCPU,
			"availMem", availMem, "allocMem", allocMem, "remainingMem", remainingMem)
	}
	memoryGB := float64(memMiB) / 1024.0

	countByCPU := maxCount
	if vCPUs > 0 {
		countByCPU = remainingVCPU / int(vCPUs)
	}

	countByMem := maxCount
	if memoryGB > 0 {
		countByMem = int(remainingMem / memoryGB)
	}

	result := min(countByMem, countByCPU)
	if requiresGPU {
		result = min(result, availGPU)
	}
	result = min(result, maxCount)
	return max(result, 0)
}

// liveMemCount clamps n by how many guests of memGB fit in live headroom
// (availMemGB − reservedMemGB). Negative headroom yields 0.
func liveMemCount(n int, availMemGB, reservedMemGB, memGB float64) int {
	if memGB <= 0 {
		return n
	}
	byLive := int((availMemGB - reservedMemGB) / memGB)
	return max(0, min(n, byLive))
}

// liveMemAdmissionEnabled reports whether the live-memory gate is enabled.
// Set SPINIFEX_ADMISSION_LIVE_MEM=0/false/off/no to disable.
func liveMemAdmissionEnabled(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("SPINIFEX_ADMISSION_LIVE_MEM"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// readMemAvailableGB returns /proc/meminfo MemAvailable in GB. Returns ok=false
// on non-Linux or read failure (fail open — accounting gate still applies).
func readMemAvailableGB() (float64, bool) {
	if runtime.GOOS != "linux" {
		return 0, false
	}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		slog.Warn("live-mem admission gate: read /proc/meminfo failed, falling back to accounting", "err", err)
		return 0, false
	}
	kb, ok := parseMemAvailableKB(data)
	if !ok {
		slog.Warn("live-mem admission gate: MemAvailable absent from /proc/meminfo, falling back to accounting")
		return 0, false
	}
	return float64(kb) / (1024 * 1024), true
}

// parseMemAvailableKB extracts MemAvailable (kB) from /proc/meminfo. Pure.
func parseMemAvailableKB(data []byte) (int64, bool) {
	for line := range strings.SplitSeq(string(data), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "MemAvailable" {
			continue
		}
		fields := strings.Fields(val) // "  12345 kB"
		if len(fields) < 1 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb, true
	}
	return 0, false
}

// Default per-volume nbdkit memory charges: main ~0.75 GB under I/O, aux ~96 MiB.
// Env-tunable via SPINIFEX_NBDKIT_{MAIN,AUX}_MIB.
const (
	defaultNbdkitMainMiB = 768
	defaultNbdkitAuxMiB  = 96
)

// Default volume layout: one main root volume + two aux volumes (cloud-init, efi).
const (
	defaultMainVolumes = 1
	defaultAuxVolumes  = 2
)

// resolveNbdkitCharge returns per-volume nbdkit charges, overridable via env.
func resolveNbdkitCharge(getenv func(string) string) (mainMiB, auxMiB int) {
	mainMiB, auxMiB = defaultNbdkitMainMiB, defaultNbdkitAuxMiB
	if v := getenv("SPINIFEX_NBDKIT_MAIN_MIB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			mainMiB = n
		} else {
			slog.Warn("ignoring SPINIFEX_NBDKIT_MAIN_MIB", "value", v, "err", err)
		}
	}
	if v := getenv("SPINIFEX_NBDKIT_AUX_MIB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			auxMiB = n
		} else {
			slog.Warn("ignoring SPINIFEX_NBDKIT_AUX_MIB", "value", v, "err", err)
		}
	}
	return mainMiB, auxMiB
}

// nbdkitChargeMiB returns the total nbdkit memory charge for mainVols + auxVols.
func nbdkitChargeMiB(mainVols, auxVols, mainMiB, auxMiB int) int64 {
	return int64(mainVols*mainMiB + auxVols*auxMiB)
}

// resourceStatsForType computes the InstanceTypeCap for a single type given
// remaining resources. Clamps negative capacity to zero without logging.
func resourceStatsForType(remainVCPU int, remainMem float64, it *ec2.InstanceTypeInfo) types.InstanceTypeCap {
	vCPUs := instanceTypeVCPUs(it)
	memGB := float64(instanceTypeMemoryMiB(it)) / 1024.0

	count := 0
	if vCPUs > 0 && memGB > 0 {
		countVCPU := remainVCPU / int(vCPUs)
		countMem := int(remainMem / memGB)
		count = max(min(countMem, countVCPU), 0)
	}

	name := ""
	if it.InstanceType != nil {
		name = *it.InstanceType
	}

	return types.InstanceTypeCap{
		Name:      name,
		VCPU:      int(vCPUs),
		MemoryGB:  memGB,
		Available: count,
	}
}

// allocateForLaunch determines the number of instances to launch given
// available capacity and the MinCount/MaxCount constraints from a
// RunInstances request. Returns the launch count or an error if the
// minimum cannot be satisfied.
func allocateForLaunch(canAlloc, minCount, maxCount int) (int, error) {
	if canAlloc < minCount {
		return 0, errInsufficientCapacity
	}
	return min(canAlloc, maxCount), nil
}
