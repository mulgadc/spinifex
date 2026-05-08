package daemon

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

func TestIsInstanceVisible(t *testing.T) {
	tests := []struct {
		name            string
		callerAccountID string
		ownerAccountID  string
		expected        bool
	}{
		{
			name:            "same account",
			callerAccountID: "123456789012",
			ownerAccountID:  "123456789012",
			expected:        true,
		},
		{
			name:            "different account",
			callerAccountID: "123456789012",
			ownerAccountID:  "999999999999",
			expected:        false,
		},
		{
			name:            "untenanted instance visible to root",
			callerAccountID: utils.GlobalAccountID,
			ownerAccountID:  "",
			expected:        true,
		},
		{
			name:            "untenanted instance hidden from non-root",
			callerAccountID: "123456789012",
			ownerAccountID:  "",
			expected:        false,
		},
		{
			name:            "root accessing owned instance",
			callerAccountID: utils.GlobalAccountID,
			ownerAccountID:  utils.GlobalAccountID,
			expected:        true,
		},
		{
			name:            "root accessing other account instance",
			callerAccountID: utils.GlobalAccountID,
			ownerAccountID:  "123456789012",
			expected:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isInstanceVisible(tt.callerAccountID, tt.ownerAccountID)
			if got != tt.expected {
				t.Errorf("isInstanceVisible(%q, %q) = %v, want %v",
					tt.callerAccountID, tt.ownerAccountID, got, tt.expected)
			}
		})
	}
}

func TestVolumeVisibleTo(t *testing.T) {
	tests := []struct {
		name            string
		tenantID        string
		callerAccountID string
		expected        bool
	}{
		{
			name:            "same tenant",
			tenantID:        "123456789012",
			callerAccountID: "123456789012",
			expected:        true,
		},
		{
			name:            "different tenant denied",
			tenantID:        "123456789012",
			callerAccountID: "999999999999",
			expected:        false,
		},
		{
			name:            "untenanted volume visible to root",
			tenantID:        "",
			callerAccountID: utils.GlobalAccountID,
			expected:        true,
		},
		{
			name:            "untenanted volume hidden from non-root tenant",
			tenantID:        "",
			callerAccountID: "123456789012",
			expected:        false,
		},
		{
			name:            "untenanted volume denied to empty caller",
			tenantID:        "",
			callerAccountID: "",
			expected:        false,
		},
		{
			name:            "tenant volume denied to empty caller",
			tenantID:        "123456789012",
			callerAccountID: "",
			expected:        false,
		},
		{
			name:            "root cannot impersonate tenant volume",
			tenantID:        "123456789012",
			callerAccountID: utils.GlobalAccountID,
			expected:        false,
		},
		{
			name:            "root accessing root-owned volume",
			tenantID:        utils.GlobalAccountID,
			callerAccountID: utils.GlobalAccountID,
			expected:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := volumeVisibleTo(tt.tenantID, tt.callerAccountID)
			if got != tt.expected {
				t.Errorf("volumeVisibleTo(%q, %q) = %v, want %v",
					tt.tenantID, tt.callerAccountID, got, tt.expected)
			}
		})
	}
}
