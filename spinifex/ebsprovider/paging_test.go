package ebsprovider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPageSizeClamp pins both list requests' PageSize to the same rule: a
// MaxResults above the cap is clamped to it, one at or below zero means "the
// caller has no preference", and anything in between is honoured verbatim.
func TestPageSizeClamp(t *testing.T) {
	tests := []struct {
		name       string
		maxResults int32
		want       int32
	}{
		{"above the cap is clamped", MaxListResults * 10, MaxListResults},
		{"one above the cap is clamped", MaxListResults + 1, MaxListResults},
		{"at the cap is honoured", MaxListResults, MaxListResults},
		{"below the cap is honoured", MaxListResults - 1, MaxListResults - 1},
		{"one is honoured", 1, 1},
		{"zero means no preference", 0, MaxListResults},
		{"negative means no preference", -1, MaxListResults},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equalf(t, tt.want, ListVolumesRequest{MaxResults: tt.maxResults}.PageSize(),
				"ListVolumes MaxResults=%d must page at %d; an unclamped page overflows one NATS message", tt.maxResults, tt.want)
			assert.Equalf(t, tt.want, ListSnapshotsRequest{MaxResults: tt.maxResults}.PageSize(),
				"ListSnapshots MaxResults=%d must page at %d; an unclamped page overflows one NATS message", tt.maxResults, tt.want)
		})
	}
}
