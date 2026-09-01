package ebsprovider_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ref is the identity builder: these tests are about which ids land on the page
// and what token follows it, not about the ref type built from them.
func ref(id string) string { return id }

// The walk each provider used to hand-roll. A token is the last id on the page,
// so resuming with it skips exactly what was already served.
func TestPage(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e"}

	tests := []struct {
		name          string
		ids           []string
		startingToken string
		pageSize      int
		wantPage      []string
		wantNext      string
	}{
		{
			name: "a short page ends the walk", ids: ids, pageSize: 10,
			wantPage: ids, wantNext: "",
		},
		{
			name: "an exact fit ends the walk with no token", ids: ids, pageSize: 5,
			wantPage: ids, wantNext: "",
		},
		{
			name: "a full page with more behind it returns the last id", ids: ids, pageSize: 2,
			wantPage: []string{"a", "b"}, wantNext: "b",
		},
		{
			name: "resuming skips the token itself", ids: ids, startingToken: "b", pageSize: 2,
			wantPage: []string{"c", "d"}, wantNext: "d",
		},
		{
			name: "the final page from a token carries no token", ids: ids, startingToken: "c", pageSize: 2,
			wantPage: []string{"d", "e"}, wantNext: "",
		},
		{
			name: "a token past the end returns nothing", ids: ids, startingToken: "z", pageSize: 2,
			wantPage: nil, wantNext: "",
		},
		{
			name: "an empty set returns nothing", ids: nil, pageSize: 2,
			wantPage: nil, wantNext: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, next := ebsprovider.Page(tt.ids, tt.startingToken, tt.pageSize, ref)

			assert.Equal(t, tt.wantPage, page)
			assert.Equal(t, tt.wantNext, next,
				"an empty token means last page; a non-empty one must be the last id served")
		})
	}
}

// Paging the whole set one item at a time must serve every id exactly once and
// terminate, which is the property a resume off-by-one breaks.
func TestPage_WalkingOneAtATimeServesEveryIDOnce(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e"}

	var seen []string
	token := ""
	for range len(ids) + 1 {
		page, next := ebsprovider.Page(ids, token, 1, ref)
		seen = append(seen, page...)
		if next == "" {
			break
		}
		token = next
	}

	require.Equal(t, ids, seen, "every id has to be served exactly once across the walk")
}

// TestPageSizeClamp pins both list requests' PageSize to the same rule: a
// MaxResults above the cap is clamped to it, one at or below zero means "the
// caller has no preference", and anything in between is honoured verbatim.
func TestPageSizeClamp(t *testing.T) {
	tests := []struct {
		name       string
		maxResults int32
		want       int32
	}{
		{"above the cap is clamped", ebsprovider.MaxListResults * 10, ebsprovider.MaxListResults},
		{"one above the cap is clamped", ebsprovider.MaxListResults + 1, ebsprovider.MaxListResults},
		{"at the cap is honoured", ebsprovider.MaxListResults, ebsprovider.MaxListResults},
		{"below the cap is honoured", ebsprovider.MaxListResults - 1, ebsprovider.MaxListResults - 1},
		{"one is honoured", 1, 1},
		{"zero means no preference", 0, ebsprovider.MaxListResults},
		{"negative means no preference", -1, ebsprovider.MaxListResults},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equalf(t, tt.want, ebsprovider.ListVolumesRequest{MaxResults: tt.maxResults}.PageSize(),
				"ListVolumes MaxResults=%d must page at %d; an unclamped page overflows one NATS message", tt.maxResults, tt.want)
			assert.Equalf(t, tt.want, ebsprovider.ListSnapshotsRequest{MaxResults: tt.maxResults}.PageSize(),
				"ListSnapshots MaxResults=%d must page at %d; an unclamped page overflows one NATS message", tt.maxResults, tt.want)
		})
	}
}
