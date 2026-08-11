package handlers_ochrevector

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClampQueryK proves Query's k default/clamp rule directly (D8/D10):
// k<=0 defaults to 10, k in range passes through, and any k above 100 is
// silently clamped rather than erroring.
func TestClampQueryK(t *testing.T) {
	tests := []struct {
		name string
		k    int
		want int
	}{
		{"zero defaults", 0, defaultQueryK},
		{"negative defaults", -5, defaultQueryK},
		{"in range passes through", 25, 25},
		{"at cap passes through", maxQueryK, maxQueryK},
		{"over cap clamps", 500, maxQueryK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, clampQueryK(tt.k))
		})
	}
}

// TestFakeBackend_Query_DefaultsAndClampsK proves the backend seam itself
// applies the same default/clamp rule Query documents, using the in-memory
// fake so this is a pure-Go test with no Postgres dependency.
func TestFakeBackend_Query_DefaultsAndClampsK(t *testing.T) {
	backend := newFakeBackend()
	ctx := context.Background()

	canned := make([]QueryResult, 150)
	for i := range canned {
		canned[i] = QueryResult{Chunk: "chunk", SourceKey: "docs/one.txt", Score: 1.0}
	}
	backend.queryResults = canned

	// k<=0 defaults to 10.
	got, err := backend.Query(ctx, "111111111111", "idx-one", []float32{1, 0}, 0, nil)
	require.NoError(t, err)
	assert.Len(t, got, defaultQueryK)
	assert.Equal(t, defaultQueryK, backend.lastQueryK)

	// A k above the hard cap is clamped, not rejected.
	got, err = backend.Query(ctx, "111111111111", "idx-one", []float32{1, 0}, 1000, nil)
	require.NoError(t, err)
	assert.Len(t, got, maxQueryK)
	assert.Equal(t, maxQueryK, backend.lastQueryK)

	// An in-range k passes straight through.
	got, err = backend.Query(ctx, "111111111111", "idx-one", []float32{1, 0}, 30, nil)
	require.NoError(t, err)
	assert.Len(t, got, 30)
}

// TestFakeBackend_Query_ResultsPlumbedThrough proves the exact QueryResult
// values set on the backend come back unchanged through the Query call --
// the plumbing this stage adds, independent of any real ANN ranking.
func TestFakeBackend_Query_ResultsPlumbedThrough(t *testing.T) {
	backend := newFakeBackend()
	ctx := context.Background()

	want := []QueryResult{
		{Chunk: "first chunk", Metadata: map[string]any{"category": "handbook"}, SourceKey: "docs/a.txt", SourceOffset: 0, Score: 0.95},
		{Chunk: "second chunk", Metadata: map[string]any{"category": "faq"}, SourceKey: "docs/b.txt", SourceOffset: 128, Score: 0.42},
	}
	backend.queryResults = want

	got, err := backend.Query(ctx, "111111111111", "idx-one", []float32{0.1, 0.2, 0.3}, 5, nil)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestFakeBackend_Query_PropagatesBackendError proves a backend failure
// surfaces to the caller rather than being swallowed.
func TestFakeBackend_Query_PropagatesBackendError(t *testing.T) {
	backend := newFakeBackend()
	backend.failQuery = errFakeBackend

	_, err := backend.Query(context.Background(), "111111111111", "idx-one", []float32{1}, 5, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errFakeBackend)
}

// TestFakeBackend_Query_RecordsFilterForAssertions proves the filter a
// caller passes reaches the backend call, so an orchestration-level test
// can assert the right filter was requested even though the fake does not
// evaluate it.
func TestFakeBackend_Query_RecordsFilterForAssertions(t *testing.T) {
	backend := newFakeBackend()
	f := Equals("category", "handbook")

	_, err := backend.Query(context.Background(), "111111111111", "idx-one", []float32{1}, 5, f)
	require.NoError(t, err)
	assert.Same(t, f, backend.lastQueryFilter)
}
