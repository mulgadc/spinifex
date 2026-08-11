package hostile_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/hostile"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/nullprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// soakDuration keeps the default short enough for CI while letting a real
// soak run for hours from the same code. A fault injector only earns its
// keep over a long run, but a long run must not be the only way to use it.
func soakDuration(t *testing.T) time.Duration {
	t.Helper()
	raw := os.Getenv("EBS_SOAK_DURATION")
	if raw == "" {
		return 2 * time.Second
	}
	parsed, err := time.ParseDuration(raw)
	require.NoError(t, err, "EBS_SOAK_DURATION")
	return parsed
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

func runSoak(t *testing.T, seed uint64, workers int, duration time.Duration) soakResult {
	t.Helper()
	inner := nullprovider.New()
	provider := hostile.New(inner, hostile.Config{
		Seed:               seed,
		ErrorRate:          0.10,
		ErrorAfterWorkRate: 0.10,
		LatencyRate:        0.05,
		UnavailableRate:    0.05,
		LieRate:            0.10,
		MaxLatency:         2 * time.Millisecond,
	})

	var mu sync.Mutex
	beliefs := map[string]belief{}
	deadline := time.Now().Add(duration)

	var wg sync.WaitGroup
	for worker := range workers {
		wg.Go(func() {
			for i := 0; time.Now().Before(deadline); i++ {
				// Each worker owns its own ID space, so faults drawn per
				// volume never depend on how the workers interleave.
				volumeID := fmt.Sprintf("vol-soak%04d%07d", worker, i)
				state := soakOneVolume(t.Context(), provider, volumeID)
				mu.Lock()
				beliefs[volumeID] = state
				mu.Unlock()
			}
		})
	}
	wg.Wait()

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
		_ = provider.UnpublishVolume(ctx, ebsprovider.UnpublishVolumeRequest{
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

// explainedBy reports whether an injection recorded for volumeID accounts for
// the provider still holding it: work that was done and then reported failed.
func explainedBy(injections []hostile.Injection, volumeID string) bool {
	for _, injection := range injections {
		if injection.Target != volumeID {
			continue
		}
		switch injection.Fault {
		case hostile.FaultErrorAfterWork, hostile.FaultLatency, hostile.FaultLie:
			return true
		}
	}
	return false
}

// TestSoak_EveryLeakIsAttributable is the oracle. Injecting faults proves
// nothing on its own; what proves something is that every volume the provider
// still holds can be traced to a fault that was injected. A leftover volume
// with no injection behind it is the control plane losing track of storage on
// its own, which is the bug worth finding.
func TestSoak_EveryLeakIsAttributable(t *testing.T) {
	result := runSoak(t, 20260811, 8, soakDuration(t))
	require.NotEmpty(t, result.injections, "the run injected nothing, so it proves nothing")
	require.NotEmpty(t, result.beliefs)

	held := map[string]bool{}
	for _, id := range result.held {
		held[id] = true
	}

	var unexplained []string
	for volumeID, state := range result.beliefs {
		switch {
		case state.deletedOK && held[volumeID]:
			unexplained = append(unexplained, volumeID+" (delete reported success, still held)")
		case !state.createdOK && held[volumeID] && !explainedBy(result.injections, volumeID):
			unexplained = append(unexplained, volumeID+" (create reported failure, held anyway)")
		case state.createdOK && !state.deletedOK && !held[volumeID] && !explainedBy(result.injections, volumeID):
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
	const seed = 99
	const duration = 500 * time.Millisecond

	first := runSoak(t, seed, 4, duration)
	second := runSoak(t, seed, 4, duration)

	// The runs cover different volumes, since each stops on a deadline. Only
	// the volumes both reached can be compared, and for those the faults must
	// be identical despite the interleaving being different.
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
