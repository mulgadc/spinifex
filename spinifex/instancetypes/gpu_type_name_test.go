package instancetypes

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGPUVendorForType(t *testing.T) {
	tests := []struct {
		instanceType string
		wantVendor   string
	}{
		{"g5.xlarge", "nvidia"},
		{"p4d.xlarge", "nvidia"},
		{"m5.large", ""},
		{"", ""},
	}
	for _, tc := range tests {
		t.Run(tc.instanceType, func(t *testing.T) {
			assert.Equal(t, tc.wantVendor, GPUVendorForType(tc.instanceType))
		})
	}
}

func TestIsGPUTypeName(t *testing.T) {
	tests := []struct {
		instanceType string
		want         bool
	}{
		{"g5.xlarge", true},
		{"p4d.xlarge", true},
		{"m5.large", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.instanceType, func(t *testing.T) {
			assert.Equal(t, tc.want, IsGPUTypeName(tc.instanceType))
		})
	}
}
