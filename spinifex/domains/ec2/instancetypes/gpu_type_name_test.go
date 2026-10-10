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
		// Both name a shape, not a family; the vendor comes from discovery.
		{"gpu.8x4c", ""},
		{"mig.1g.10gb", ""},
	}
	for _, tc := range tests {
		t.Run(tc.instanceType, func(t *testing.T) {
			assert.Equal(t, tc.wantVendor, GPUVendorForType(tc.instanceType))
		})
	}
}

// A false answer routes ECS and EKS to the non-GPU AMI, which boots the node
// with its GPUs passed through and no driver. The vendor being unknowable from
// the name is not a reason to call these types CPU-only.
func TestIsGPUTypeName(t *testing.T) {
	tests := []struct {
		instanceType string
		want         bool
	}{
		{"g5.xlarge", true},
		{"p4d.xlarge", true},
		{"m5.large", false},
		{"", false},
		{"gpu.1x2c", true},
		{"gpu.8x8c", true},
		{"mig.1g.10gb", true},
	}
	for _, tc := range tests {
		t.Run(tc.instanceType, func(t *testing.T) {
			assert.Equal(t, tc.want, IsGPUTypeName(tc.instanceType))
		})
	}
}
