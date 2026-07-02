package handlers_ecs

import (
	"errors"
	"sort"
	"strings"
)

// Placement strategy identifiers (ecs-v1.md Q15). Default is binpack:memory.
const (
	StrategyBinpack = "binpack"
	StrategySpread  = "spread"
	StrategyRandom  = "random"
)

// ErrNoCapacity is returned when no ACTIVE instance can fit the task.
var ErrNoCapacity = errors.New("no container instance has capacity for the task")

// remainingCPU/remainingMemory report an instance's unreserved capacity.
func (r *InstanceRecord) remainingCPU() int    { return r.TotalCPU - r.ReservedCPU }
func (r *InstanceRecord) remainingMemory() int { return r.TotalMemoryMiB - r.ReservedMemoryMiB }

// fits reports whether the instance is ACTIVE and has room for the reservation.
func (r *InstanceRecord) fits(cpu, mem int) bool {
	if r.Status != InstanceStatusActive {
		return false
	}
	return r.remainingCPU() >= cpu && r.remainingMemory() >= mem
}

// placeTask selects a container instance for a task reserving (cpu, mem) using
// the requested strategy. Candidates are filtered to ACTIVE instances that fit;
// ties broken by instance ID for determinism. strategy "" defaults to binpack.
//
// binpack: pick the instance with the LEAST remaining memory that still fits
// (tightest pack). spread: pick the MOST remaining memory (widest spread).
// random: caller-stable first fit by instance ID.
func placeTask(instances []InstanceRecord, cpu, mem int, strategy string) (*InstanceRecord, error) {
	candidates := make([]InstanceRecord, 0, len(instances))
	for _, inst := range instances {
		if inst.fits(cpu, mem) {
			candidates = append(candidates, inst)
		}
	}
	if len(candidates) == 0 {
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
