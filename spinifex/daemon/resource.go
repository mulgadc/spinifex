package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/types"
)

// errInsufficientCapacity is returned by allocateForLaunch when MinCount
// cannot be satisfied.
var errInsufficientCapacity = errors.New("insufficient capacity to satisfy MinCount")

// hostReserve is the fixed amount of host CPU and RAM held back from guest
// scheduling so the spinifex daemon and co-located services (NATS,
// predastore, viperblock, vpcd, awsgw, ui) cannot be starved by guest VMs
// at maximum density. A future `capacity` command will lift this into
// operator-tunable config.
type hostReserve struct {
	vCPU  int
	memGB float64
}

// defaultHostReserve is sized to cover predastore + viperblock under load;
// the daemon itself needs little.
var defaultHostReserve = hostReserve{vCPU: 2, memGB: 2.0}

// resolveHostReserve returns defaultHostReserve, with vCPU/memGB overridden
// when SPINIFEX_RESERVED_VCPU / SPINIFEX_RESERVED_MEM_GB are set to valid
// non-negative values. Invalid values are logged and ignored — keeps a
// typo'd env from silently widening the reserve. Intended as a stop-gap
// until the planned operator-tunable [capacity] config lands.
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

// minHostMemHeadroomGB is the minimum schedulable memory we require above
// the reserve, so a host that just meets the reserve still has a small
// amount left to launch the smallest guest type.
const minHostMemHeadroomGB = 0.5

// applyHostReserve validates that the host meets the minimum size for the
// given reserve and returns the reserve to apply. Pure function — no locks
// or side effects. Exists as a helper for unit-testability of the
// validation bounds.
//
// vCPU and memory are checked separately so the returned error names the
// failing dimension, letting alerting/log filters distinguish a CPU
// shortfall from a memory shortfall.
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

// canAllocateCount returns how many instances of the given type can fit
// in the remaining capacity, capped at maxCount. Pure aside from a single
// slog.Error when remaining capacity is negative — that condition would
// otherwise be silently clamped to zero, hiding a misconfigured reserve
// or allocation accounting drift.
//
// availGPU is the number of free GPUs in the pool; requiresGPU indicates
// that this instance type needs one. When requiresGPU is true and availGPU
// is zero, the result is always 0.
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

// resourceStatsForType computes the InstanceTypeCap for a single instance type
// given the remaining host resources. Pure function — no locks or side effects.
// Callers are responsible for alarming on negative remainVCPU/remainMem
// (see canAllocateCount); resourceStatsForType silently clamps to zero
// because it's invoked in a per-type loop and would otherwise log N times.
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
