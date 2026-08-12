package hostile_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/conformance"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/hostile"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/nullprovider"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const volumeBytes = 1 << 30

// countingProvider records how many times the inner provider was actually
// reached, which is the only way to tell FaultError (nothing happened) from
// FaultErrorAfterWork (it happened and was reported as failed).
type countingProvider struct {
	ebsprovider.EBSProvider

	creates int
	deletes int
}

var _ ebsprovider.EBSProvider = (*countingProvider)(nil)

func (c *countingProvider) CreateVolume(ctx context.Context, req ebsprovider.CreateVolumeRequest) (*ebsprovider.Volume, error) {
	c.creates++
	return c.EBSProvider.CreateVolume(ctx, req)
}

func (c *countingProvider) DeleteVolume(ctx context.Context, req ebsprovider.DeleteVolumeRequest) error {
	c.deletes++
	return c.EBSProvider.DeleteVolume(ctx, req)
}

func createVolume(ctx context.Context, provider ebsprovider.EBSProvider, volumeID string) (*ebsprovider.Volume, error) {
	return provider.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned:     ebsprovider.NewVersioned(),
		VolumeID:      volumeID,
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: volumeBytes},
	})
}

// drive runs a fixed sequence of calls, so two runs differ only in the seed.
func drive(t *testing.T, provider ebsprovider.EBSProvider, count int) {
	t.Helper()
	for i := range count {
		volumeID := fmt.Sprintf("vol-drive%011d", i)
		_, _ = createVolume(t.Context(), provider, volumeID)
		_, _ = provider.GetVolume(t.Context(), ebsprovider.GetVolumeRequest{
			Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
		})
		_ = provider.DeleteVolume(t.Context(), ebsprovider.DeleteVolumeRequest{
			Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID,
		})
	}
}

func hostileConfig(seed uint64) hostile.Config {
	return hostile.Config{
		Seed:               seed,
		ErrorRate:          0.15,
		ErrorAfterWorkRate: 0.15,
		LatencyRate:        0.10,
		UnavailableRate:    0.10,
		LieRate:            0.15,
	}
}

// TestZeroConfigInjectsNothing is the default that matters: wrapping a
// provider must not hurt it until a caller asks.
func TestZeroConfigInjectsNothing(t *testing.T) {
	provider := hostile.New(nullprovider.New(), hostile.Config{})
	drive(t, provider, 50)
	assert.Empty(t, provider.Injections())

	volume, err := createVolume(t.Context(), provider, "vol-zeroconfig00001")
	require.NoError(t, err)
	assert.Equal(t, int64(volumeBytes), volume.CapacityBytes)
}

// TestPassThroughIsConformant holds the decorator to the same contract it
// decorates. Every difference a run shows must come from a fault that was
// asked for, never from the wrapping.
func TestPassThroughIsConformant(t *testing.T) {
	conformance.RunSuite(t, func(t *testing.T) ebsprovider.EBSProvider {
		return hostile.New(ebsprovider.NewMemoryProvider(conformance.ReferenceCapabilities), hostile.Config{})
	})
}

// TestSameSeedReplaysTheSameFaults is the whole reason this is seeded. A soak
// failure at hour six has to be reproducible in a test, and it can only be if
// the fault sequence is a function of the seed and the calls.
func TestSameSeedReplaysTheSameFaults(t *testing.T) {
	first := hostile.New(nullprovider.New(), hostileConfig(42))
	second := hostile.New(nullprovider.New(), hostileConfig(42))
	drive(t, first, 40)
	drive(t, second, 40)

	require.NotEmpty(t, first.Injections(), "the run injected nothing, so it proves nothing")
	assert.Equal(t, first.Injections(), second.Injections())
}

func TestDifferentSeedsDiverge(t *testing.T) {
	first := hostile.New(nullprovider.New(), hostileConfig(42))
	second := hostile.New(nullprovider.New(), hostileConfig(43))
	drive(t, first, 40)
	drive(t, second, 40)

	assert.NotEqual(t, first.Injections(), second.Injections())
}

// TestEveryFaultClassIsReachable guards against a threshold arithmetic slip
// that would silently disable a class: a fault nobody can draw is a fault
// nobody tests against.
func TestEveryFaultClassIsReachable(t *testing.T) {
	provider := hostile.New(nullprovider.New(), hostileConfig(7))
	drive(t, provider, 400)

	seen := map[hostile.Fault]bool{}
	for _, injection := range provider.Injections() {
		seen[injection.Fault] = true
		assert.NotEmpty(t, injection.Detail, "%s has no detail to reproduce it from", injection)
	}
	for _, fault := range []hostile.Fault{
		hostile.FaultError,
		hostile.FaultErrorAfterWork,
		hostile.FaultLatency,
		hostile.FaultUnavailable,
		hostile.FaultLie,
	} {
		assert.True(t, seen[fault], "%s was never drawn", fault)
	}
}

// TestFaultErrorNeverReachesTheProvider and its ErrorAfterWork counterpart pin
// the distinction the two faults exist for.
func TestFaultErrorNeverReachesTheProvider(t *testing.T) {
	counting := &countingProvider{EBSProvider: nullprovider.New()}
	provider := hostile.New(counting, hostile.Config{Seed: 1, ErrorRate: 1})

	_, err := createVolume(t.Context(), provider, "vol-neverreaches001")
	require.Error(t, err)
	assert.Zero(t, counting.creates, "the provider was reached for a call that was supposed to fail first")
}

// TestFaultErrorAfterWorkLeavesTheVolumeBehind is the orphan-making case, and
// the reason an enumeration-based scan is the right oracle for a soak: the
// caller was told the create failed, and the volume exists.
func TestFaultErrorAfterWorkLeavesTheVolumeBehind(t *testing.T) {
	inner := nullprovider.New()
	counting := &countingProvider{EBSProvider: inner}
	provider := hostile.New(counting, hostile.Config{Seed: 1, ErrorAfterWorkRate: 1})

	const volumeID = "vol-leftbehind00001"
	_, err := createVolume(t.Context(), provider, volumeID)
	require.Error(t, err, "the caller must be told this failed")
	assert.Equal(t, 1, counting.creates, "the work must actually have happened")

	// The oracle reads the provider directly. Asking through the injector
	// would be asking the thing under test whether it lied.
	resp, err := inner.ListVolumes(t.Context(), ebsprovider.ListVolumesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)
	var ids []string
	for _, ref := range resp.Volumes {
		ids = append(ids, ref.ID)
	}
	assert.Contains(t, ids, volumeID, "the provider holds a volume the caller believes was never created")
}

// TestUnavailableIsRetryable matters because owner-first routing treats
// no-responders as safe to retry elsewhere and a timeout as not.
func TestUnavailableIsRetryable(t *testing.T) {
	provider := hostile.New(nullprovider.New(), hostile.Config{Seed: 1, UnavailableRate: 1})
	_, err := createVolume(t.Context(), provider, "vol-unavailable0001")
	require.ErrorIs(t, err, nats.ErrNoResponders)
}

// aliasingProvider hands back a pointer into its own state, the way a
// provider that forgot to copy would. MemoryProvider returns copies, so a
// lie told through it cannot corrupt anything and proves nothing about
// whether the injector copies.
type aliasingProvider struct {
	ebsprovider.EBSProvider

	volume *ebsprovider.Volume
}

var _ ebsprovider.EBSProvider = (*aliasingProvider)(nil)

func (a *aliasingProvider) CreateVolume(ctx context.Context, req ebsprovider.CreateVolumeRequest) (*ebsprovider.Volume, error) {
	volume, err := a.EBSProvider.CreateVolume(ctx, req)
	if err != nil {
		return nil, err
	}
	a.volume = volume
	return a.volume, nil
}

// TestLiesDoNotCorruptTheProvider keeps the fault a fault. If a wrong answer
// also changed what the provider held, the oracle would be measuring the
// injector's bugs instead of the control plane's.
func TestLiesDoNotCorruptTheProvider(t *testing.T) {
	aliasing := &aliasingProvider{EBSProvider: nullprovider.New()}
	provider := hostile.New(aliasing, hostile.Config{Seed: 3, LieRate: 1})

	const volumeID = "vol-lieaboutit00001"
	lied, err := createVolume(t.Context(), provider, volumeID)
	require.NoError(t, err)

	require.NotNil(t, aliasing.volume)
	assert.Equal(t, int64(volumeBytes), aliasing.volume.CapacityBytes, "the lie was written into the provider's own object")
	assert.NotEqual(t, *aliasing.volume, *lied, "a lie that matches the truth is not a lie")
}

// TestListVolumesNeverLies protects the oracle. A soak checks its invariants
// with enumeration, so an enumeration that can be made to agree with a broken
// control plane cannot falsify anything.
func TestListVolumesNeverLies(t *testing.T) {
	provider := hostile.New(nullprovider.New(), hostile.Config{Seed: 5, LieRate: 1})
	drive(t, provider, 30)
	for range 30 {
		_, err := provider.ListVolumes(t.Context(), ebsprovider.ListVolumesRequest{Versioned: ebsprovider.NewVersioned()})
		require.NoError(t, err)
	}
	for _, injection := range provider.Injections() {
		assert.NotEqual(t, "volume.list", injection.Verb, "enumeration was faulted: %s", injection)
	}
}

// TestLatencyRespectsCancellation stops a long delay from outliving the caller
// that gave up on it, which is what would make a soak run leak goroutines.
func TestLatencyRespectsCancellation(t *testing.T) {
	provider := hostile.New(nullprovider.New(), hostile.Config{
		Seed: 1, LatencyRate: 1, MaxLatency: time.Hour,
	})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := createVolume(ctx, provider, "vol-cancelme0000001")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 5*time.Second)
}
