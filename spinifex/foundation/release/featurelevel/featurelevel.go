// Package featurelevel is the cluster feature level: a recorded integer that
// advances once every member runs a build supporting it and only a rollback
// lowers. It imports no domain; a feature supplies its own drain check.
package featurelevel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// Build is the highest level this binary supports. Any change that adds an
// N-only semantic raises it and declares the level below.
const Build = 1

// Level names one N-only semantic.
type Level struct {
	N    int
	Name string
}

// Baseline is level 1: this build's first release, with no N-only semantic.
const Baseline = 1

var declared = []Level{{N: Baseline, Name: "baseline"}}

// Levels returns every declared level in ascending order.
func Levels() []Level { return slices.Clone(declared) }

// DrainCheck reports how many obligations of a level are still outstanding, so
// a rollback can wait for them instead of discarding or reinterpreting them.
type DrainCheck func(ctx context.Context) (outstanding int, err error)

// NothingToDrain is the check for a level that creates no obligations.
func NothingToDrain(context.Context) (int, error) { return 0, nil }

var (
	drainMu sync.Mutex
	drains  = map[int]DrainCheck{Baseline: NothingToDrain}
)

// RegisterDrain declares level n's one drain check. A level that is not
// declared, or already has a check, is a programming error.
func RegisterDrain(n int, check DrainCheck) error {
	drainMu.Lock()
	defer drainMu.Unlock()
	if !slices.ContainsFunc(declared, func(l Level) bool { return l.N == n }) {
		return fmt.Errorf("featurelevel: level %d is not declared", n)
	}
	if check == nil {
		return fmt.Errorf("featurelevel: level %d drain check is nil", n)
	}
	if _, dup := drains[n]; dup {
		return fmt.Errorf("featurelevel: level %d already has a drain check", n)
	}
	drains[n] = check
	return nil
}

// DrainChecks returns the registered checks for every level above target.
func DrainChecks(target int) map[int]DrainCheck {
	drainMu.Lock()
	defer drainMu.Unlock()
	out := map[int]DrainCheck{}
	for n, c := range drains {
		if n > target {
			out[n] = c
		}
	}
	return out
}

// UncoveredLevels lists the declared levels with no drain check. It must be
// empty at the composition root.
func UncoveredLevels() []int {
	drainMu.Lock()
	defer drainMu.Unlock()
	return uncovered(declared, drains)
}

func uncovered(levels []Level, checks map[int]DrainCheck) []int {
	var missing []int
	for _, l := range levels {
		if checks[l.N] == nil {
			missing = append(missing, l.N)
		}
	}
	return missing
}

var (
	// ErrCLITooOld is returned before anything is written when the recorded
	// level is above the caller's Build: it cannot know the newer levels' drains.
	ErrCLITooOld = errors.New("featurelevel: recorded level is above this build")
	// ErrNoRollback is returned by a rollback step with no rollback recorded.
	ErrNoRollback = errors.New("featurelevel: no rollback in progress")
	// ErrRollbackInProgress is returned when a different rollback is already recorded.
	ErrRollbackInProgress = errors.New("featurelevel: a different rollback is in progress")
	// ErrBadTarget is returned for a rollback target that is not below the level.
	ErrBadTarget = errors.New("featurelevel: rollback target must be below the recorded level and not negative")
)
