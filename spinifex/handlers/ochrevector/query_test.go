// Exercises unexported query clamping internals with no exported surface
// to drive them through.
//
//test:in-package
package handlers_ochrevector

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
