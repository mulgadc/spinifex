package ochre

import (
	"testing"
	"time"

	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const evictionVictimModel = "other.model-v1:0"

// rewriteVictim replaces the victim's stored record with mutate's result,
// standing in for a writer that lands between selection and teardown.
func rewriteVictim(t *testing.T, s *Service, mutate func(*EndpointRecord)) {
	t.Helper()
	key := EndpointKey(awsidentifiers.GlobalAccountID, evictionVictimModel)
	rec, rev, found, err := s.store.getRevision(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)
	mutate(&rec)
	_, err = s.store.CompareAndSet(t.Context(), key, &rec, rev)
	require.NoError(t, err)
}

// A selection gone stale before teardown must leave the endpoint in place:
// a replacement incarnation, a pin, or any later write supersedes it.
func TestEvictForCapacity_StaleSelectionIsNotTornDown(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*EndpointRecord){
		"replaced by a pinned incarnation": func(rec *EndpointRecord) {
			rec.InstanceID = "i-replacement"
			rec.CreatedAt = rec.CreatedAt.Add(time.Minute)
			rec.Pinned = true
			rec.Generation++
		},
		"same incarnation pinned": func(rec *EndpointRecord) {
			rec.Pinned = true
			rec.Generation++
		},
		"generation moved on": func(rec *EndpointRecord) {
			rec.Generation++
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newLaunchHarness()
			s, _ := newTestService(t, h, 200, exhaustedGPU())
			seedReadyEndpoint(t, s, evictionVictimModel, time.Now().UTC().Add(-time.Hour))

			var selected []string
			s.afterEvictionSelect = func(victim EndpointRecord) {
				selected = append(selected, victim.ModelID)
				rewriteVictim(t, s, mutate)
			}

			assert.False(t, s.evictForCapacity(t.Context(), testModelID, 5120))
			assert.Equal(t, []string{evictionVictimModel}, selected, "the stale record must have been the one selected")
			assert.Empty(t, h.launcher.terminations(), "a superseded selection must not tear anything down")

			desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: evictionVictimModel}, "")
			require.NoError(t, err)
			assert.Equal(t, StateReady, desc.Endpoint.State)
		})
	}
}

// The unchanged victim is still evicted through the same guarded path.
func TestEvictForCapacity_UnchangedSelectionIsEvicted(t *testing.T) {
	t.Parallel()
	h := newLaunchHarness()
	s, _ := newTestService(t, h, 200, exhaustedGPU())
	seedReadyEndpoint(t, s, evictionVictimModel, time.Now().UTC().Add(-time.Hour))

	var selected int
	s.afterEvictionSelect = func(EndpointRecord) { selected++ }

	assert.True(t, s.evictForCapacity(t.Context(), testModelID, 5120))
	assert.Equal(t, 1, selected)
	assert.Equal(t, []string{"i-other"}, h.launcher.terminations())

	desc, err := s.Describe(t.Context(), &DescribeEndpointInput{ModelID: evictionVictimModel}, "")
	require.NoError(t, err)
	assert.Equal(t, StateAbsent, desc.Endpoint.State)
}
