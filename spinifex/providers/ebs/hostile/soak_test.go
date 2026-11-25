package hostile_test

import (
	"context"
	"fmt"
	"maps"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/hostile"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/nullprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// soakBudget bounds a run by a fixed volume count per worker, or by wall-clock
// time when duration is set. A count gives the same coverage on a loaded or
// slow machine, where a deadline gives less.
type soakBudget struct {
	volumesPerWorker int
	duration         time.Duration
	maxLatency       time.Duration
}

// countBudget keeps latency faults short: a soak with no caller deadline only
// needs them to perturb the interleaving, not to wait out real time.
func countBudget(volumesPerWorker int) soakBudget {
	return soakBudget{volumesPerWorker: volumesPerWorker, maxLatency: 50 * time.Microsecond}
}

// soakFromEnv keeps the default short enough for CI while letting a real
// soak run for hours from the same code. A fault injector only earns its
// keep over a long run, but a long run must not be the only way to use it.
func soakFromEnv(t *testing.T) soakBudget {
	t.Helper()
	raw := os.Getenv("EBS_SOAK_DURATION")
	if raw == "" {
		return countBudget(3600)
	}
	parsed, err := time.ParseDuration(raw)
	require.NoError(t, err, "EBS_SOAK_DURATION")
	return soakBudget{duration: parsed, maxLatency: 2 * time.Millisecond}
}

// belief is what the caller was told about a volume, which is all a control
// plane ever has. The provider's own answer is the truth it is checked
// against.
type belief struct {
	createdOK bool
	deletedOK bool
}

// soakResult is what one run produced, so two runs can be compared.
type soakResult struct {
	beliefs    map[string]belief
	injections []hostile.Injection
	held       []string
}

func runSoak(t *testing.T, seed uint64, workers int, budget soakBudget) soakResult {
	t.Helper()
	inner := nullprovider.New()
	provider := hostile.New(inner, hostile.Config{
		Seed:               seed,
		ErrorRate:          0.10,
		ErrorAfterWorkRate: 0.10,
		LatencyRate:        0.05,
		UnavailableRate:    0.05,
		LieRate:            0.10,
		MaxLatency:         budget.maxLatency,
	})

	perWorker := make([]map[string]belief, workers)
	deadline := time.Now().Add(budget.duration)
	more := func(i int) bool {
		if budget.volumesPerWorker > 0 {
			return i < budget.volumesPerWorker
		}
		return time.Now().Before(deadline)
	}

	var wg sync.WaitGroup
	for worker := range workers {
		local := map[string]belief{}
		perWorker[worker] = local
		wg.Go(func() {
			for i := 0; more(i); i++ {
				// Each worker owns its own ID space, so faults drawn per
				// volume never depend on how the workers interleave.
				volumeID := fmt.Sprintf("vol-soak%04d%07d", worker, i)
				local[volumeID] = soakOneVolume(t.Context(), provider, volumeID)
			}
		})
	}
	wg.Wait()

	beliefs := map[string]belief{}
	for _, local := range perWorker {
		maps.Copy(beliefs, local)
	}

	return soakResult{
		beliefs:    beliefs,
		injections: provider.Injections(),
		held:       drainHeldVolumes(t, inner),
	}
}

// soakOneVolume drives one volume through its whole life without retrying.
// Retries would hide leaks, and what is being looked for here is what the
// control plane loses track of when a provider misbehaves once.
func soakOneVolume(ctx context.Context, provider ebsprovider.EBSProvider, volumeID string) belief {
	var state belief
	if _, err := provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:     ebsprovider.NewVersioned(),
		VolumeID:      volumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: volumeBytes},
	}); err != nil {
		return state
	}
	state.createdOK = true

	_, _ = provider.GetVolume(ctx, ebsprovider.GetVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
	})
	if _, err := provider.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: "node-1",
	}); err == nil {
		_, _ = provider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{
			Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, NodeID: "node-1",
		})
	}

	if err := provider.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
	}); err == nil {
		state.deletedOK = true
	}
	return state
}

func drainHeldVolumes(t *testing.T, provider ebsprovider.EBSProvider) []string {
	t.Helper()
	var ids []string
	var token string
	for range 1000 {
		resp, err := provider.ListVolumes(context.Background(), ebsprovider.ListVolumesRequest{
			Versioned: ebsprovider.NewVersioned(), StartingToken: token,
		})
		require.NoError(t, err)
		for _, ref := range resp.Volumes {
			ids = append(ids, ref.ID)
		}
		if resp.NextToken == "" {
			sort.Strings(ids)
			return ids
		}
		token = resp.NextToken
	}
	t.Fatal("ListVolumes did not terminate")
	return nil
}

// explainedTargets is the set of volumes whose recorded injections account for
// the provider still holding them: work that was done and then reported failed.
// It is built once because a soak logs tens of thousands of injections.
func explainedTargets(injections []hostile.Injection) map[string]bool {
	explained := map[string]bool{}
	for _, injection := range injections {
		switch injection.Fault {
		case hostile.FaultErrorAfterWork, hostile.FaultLatency, hostile.FaultLie:
			explained[injection.Target] = true
		case hostile.FaultNone, hostile.FaultError, hostile.FaultUnavailable:
			// These do no work the caller is not told about, so they explain nothing.
		}
	}
	return explained
}

// TestSoak_EveryLeakIsAttributable is the oracle. Injecting faults proves
// nothing on its own; what proves something is that every volume the provider
// still holds can be traced to a fault that was injected. A leftover volume
// with no injection behind it is the control plane losing track of storage on
// its own, which is the bug worth finding.
func TestSoak_EveryLeakIsAttributable(t *testing.T) {
	t.Parallel()
	result := runSoak(t, 20260811, 8, soakFromEnv(t))
	require.NotEmpty(t, result.injections, "the run injected nothing, so it proves nothing")
	require.NotEmpty(t, result.beliefs)

	held := map[string]bool{}
	for _, id := range result.held {
		held[id] = true
	}
	explained := explainedTargets(result.injections)

	var unexplained []string
	for volumeID, state := range result.beliefs {
		switch {
		case state.deletedOK && held[volumeID]:
			unexplained = append(unexplained, volumeID+" (delete reported success, still held)")
		case !state.createdOK && held[volumeID] && !explained[volumeID]:
			unexplained = append(unexplained, volumeID+" (create reported failure, held anyway)")
		case state.createdOK && !state.deletedOK && !held[volumeID] && !explained[volumeID]:
			unexplained = append(unexplained, volumeID+" (created and not deleted, gone anyway)")
		}
	}
	sort.Strings(unexplained)
	assert.Empty(t, unexplained, "volumes the injection log cannot account for")
}

// TestSoak_ConcurrentRunsWithTheSameSeedMatch is what makes a long run worth
// running: reproducing hour six needs the faults to be a function of the seed
// and the calls, not of how the workers happened to interleave.
func TestSoak_ConcurrentRunsWithTheSameSeedMatch(t *testing.T) {
	t.Parallel()
	const seed = 99
	budget := countBudget(1400)

	first := runSoak(t, seed, 4, budget)
	second := runSoak(t, seed, 4, budget)

	// Only the volumes both runs reached can be compared, and for those the
	// faults must be identical despite the interleaving being different.
	shared := 0
	firstByVolume := indexInjections(first.injections)
	secondByVolume := indexInjections(second.injections)
	for volumeID, injections := range firstByVolume {
		other, ok := secondByVolume[volumeID]
		if !ok {
			continue
		}
		shared++
		assert.Equal(t, injections, other, "volume %s drew different faults across runs with the same seed", volumeID)
	}
	require.Positive(t, shared, "the runs shared no volumes, so nothing was compared")
}

func indexInjections(injections []hostile.Injection) map[string][]hostile.Injection {
	byVolume := map[string][]hostile.Injection{}
	for _, injection := range injections {
		byVolume[injection.Target] = append(byVolume[injection.Target], injection)
	}
	for _, list := range byVolume {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Verb != list[j].Verb {
				return list[i].Verb < list[j].Verb
			}
			return list[i].Sequence < list[j].Sequence
		})
	}
	return byVolume
}
