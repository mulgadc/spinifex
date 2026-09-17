package handlers_ecs

import (
	"errors"
	"math"
	"sort"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/instancetypes"
)

// Placement strategy identifiers (ecs-v1.md Q15). Default is binpack:memory.
const (
	StrategyBinpack = "binpack"
	StrategySpread  = "spread"
	StrategyRandom  = "random"
)

// ErrNoCapacity is returned when no ACTIVE instance can fit the task.
var ErrNoCapacity = errors.New("no container instance has capacity for the task")

// ErrNoENICapacity is returned when the only thing standing between the task and
// an instance is an awsvpc network interface slot. It is separated from
// ErrNoCapacity so the caller can say which resource bound, the way AWS does
// with RESOURCE:ENI, instead of reporting a generic placement failure.
var ErrNoENICapacity = errors.New("no container instance has a free network interface for the task")

// remainingCPU/remainingMemory/remainingGPU report an instance's unreserved capacity.
func (r *InstanceRecord) remainingCPU() int    { return r.TotalCPU - r.ReservedCPU }
func (r *InstanceRecord) remainingMemory() int { return r.TotalMemoryMiB - r.ReservedMemoryMiB }
func (r *InstanceRecord) remainingGPU() int    { return r.TotalGPU - r.ReservedGPU }

// remainingGPUIDs returns the instance's free GPU device UUIDs: GPUIDs with the
// first ReservedGPU entries dropped. Which UUID a given task actually holds is
// the agent's local ledger's call (reported back per-task); this is only a
// count-consistent view of the instance's total inventory.
func (r *InstanceRecord) remainingGPUIDs() []string {
	if r.ReservedGPU >= len(r.GPUIDs) {
		return nil
	}
	return r.GPUIDs[r.ReservedGPU:]
}

// totalENIs is the instance's awsvpc task-ENI capacity: the hot-plug slots its
// instance type carries, which is what the daemon pre-allocates PCIe root ports
// for at boot. Derived rather than stored so a corrected limit table applies at
// once. Zero means the instance type is not known — see remainingENIs.
func (r *InstanceRecord) totalENIs() int {
	if r.InstanceType == "" {
		return 0
	}
	return instancetypes.HotPlugENISlotsForType(r.InstanceType)
}

// remainingENIs reports the instance's free task-ENI slots. An instance whose
// type has not been reported is treated as unbounded: a recognised type always
// has at least one slot, so zero can only mean unknown, and refusing every
// awsvpc placement against an agent too old to report its type would turn a
// mixed-version cluster into an outage.
func (r *InstanceRecord) remainingENIs() int {
	total := r.totalENIs()
	if total == 0 {
		return math.MaxInt
	}
	return total - r.ReservedENIs
}

// fits reports whether the instance is ACTIVE and has room for the reservation.
func (r *InstanceRecord) fits(cpu, mem, gpu, eni int) bool {
	if r.Status != InstanceStatusActive {
		return false
	}
	return r.remainingCPU() >= cpu && r.remainingMemory() >= mem &&
		r.remainingGPU() >= gpu && r.remainingENIs() >= eni
}

// placeTask selects a container instance for a task reserving (cpu, mem, gpu)
// using the requested strategy. Candidates are filtered to ACTIVE instances that
// fit; ties broken by instance ID for determinism. strategy "" defaults to binpack.
//
// binpack: pick the instance with the LEAST remaining memory that still fits
// (tightest pack). spread: pick the MOST remaining memory (widest spread).
// random: caller-stable first fit by instance ID. GPU is a fit gate only; it does
// not participate in the memory-based sort (non-GPU tasks request gpu=0 and are
// unaffected).
func placeTask(instances []InstanceRecord, cpu, mem, gpu, eni int, strategy string) (*InstanceRecord, error) {
	candidates := make([]InstanceRecord, 0, len(instances))
	for _, inst := range instances {
		if inst.fits(cpu, mem, gpu, eni) {
			candidates = append(candidates, inst)
		}
	}
	if len(candidates) == 0 {
		// An instance that would have taken the task but for its network
		// interfaces makes this a distinct refusal, so the caller can name the
		// resource that bound rather than report a bare placement failure.
		if eni > 0 {
			for _, inst := range instances {
				if inst.fits(cpu, mem, gpu, 0) {
					return nil, ErrNoENICapacity
				}
			}
		}
		return nil, ErrNoCapacity
	}

	switch normalizeStrategy(strategy) {
	case StrategySpread:
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].remainingMemory() != candidates[j].remainingMemory() {
				return candidates[i].remainingMemory() > candidates[j].remainingMemory()
			}
			return candidates[i].InstanceID < candidates[j].InstanceID
		})
	case StrategyRandom:
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].InstanceID < candidates[j].InstanceID
		})
	default: // binpack
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].remainingMemory() != candidates[j].remainingMemory() {
				return candidates[i].remainingMemory() < candidates[j].remainingMemory()
			}
			return candidates[i].InstanceID < candidates[j].InstanceID
		})
	}

	chosen := candidates[0]
	return &chosen, nil
}

// normalizeStrategy maps "binpack:memory"/"binpack:cpu" → "binpack" and lowercases.
func normalizeStrategy(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	switch s {
	case StrategySpread, StrategyRandom:
		return s
	default:
		return StrategyBinpack
	}
}
