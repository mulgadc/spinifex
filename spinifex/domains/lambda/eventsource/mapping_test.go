package eventsource_test

import (
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/eventsource"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_RevisionsGuardEveryChange(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	s, err := eventsource.Open(t.Context(), js)
	require.NoError(t, err)

	entries, err := s.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, entries)

	m := eventsource.Mapping{FormatVersion: eventsource.FormatVersion, UUID: "u1", State: eventsource.StateEnabled}
	require.NoError(t, s.Create(t.Context(), m))
	require.ErrorIs(t, s.Create(t.Context(), m), eventsource.ErrConflict)

	got, rev, err := s.Get(t.Context(), "u1")
	require.NoError(t, err)
	assert.Equal(t, m, got)

	m.State = eventsource.StateDeleting
	newRev, err := s.Update(t.Context(), m, rev)
	require.NoError(t, err)
	_, err = s.Update(t.Context(), m, rev)
	require.ErrorIs(t, err, eventsource.ErrConflict, "a stale revision never overwrites")
	require.ErrorIs(t, s.Remove(t.Context(), "u1", rev), eventsource.ErrConflict, "a stale revision never removes")

	require.NoError(t, s.Remove(t.Context(), "u1", newRev))
	_, _, err = s.Get(t.Context(), "u1")
	require.ErrorIs(t, err, eventsource.ErrNotFound)
	entries, err = s.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, entries)
}
