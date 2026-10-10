package identifiers

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateResourceID(t *testing.T) {
	tests := []struct {
		prefix string
	}{
		{"i"},
		{"r"},
		{"vol"},
		{"snap"},
		{"key"},
		{"eigw"},
		{"ami"},
	}

	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			id := GenerateResourceID(tt.prefix)
			assert.True(t, strings.HasPrefix(id, tt.prefix+"-"))
			// prefix + "-" + 17 hex chars
			assert.Len(t, id, len(tt.prefix)+1+17)

			// Verify uniqueness
			id2 := GenerateResourceID(tt.prefix)
			assert.NotEqual(t, id, id2)
		})
	}
}
