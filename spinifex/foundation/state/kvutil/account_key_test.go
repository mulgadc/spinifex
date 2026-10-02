package kvutil

import "testing"

func TestAccountKey(t *testing.T) {
	tests := []struct {
		accountID  string
		resourceID string
		want       string
	}{
		{"000000000000", "vpc-123", "000000000000.vpc-123"},
		{"123456789012", "igw-abc", "123456789012.igw-abc"},
		{"", "vol-1", ".vol-1"},
	}
	for _, tt := range tests {
		got := AccountKey(tt.accountID, tt.resourceID)
		if got != tt.want {
			t.Errorf("AccountKey(%q, %q) = %q, want %q", tt.accountID, tt.resourceID, got, tt.want)
		}
	}
}
