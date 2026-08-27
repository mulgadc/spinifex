package ebsprovider_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
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
