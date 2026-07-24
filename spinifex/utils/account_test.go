package utils

import (
	"testing"
)

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

func TestIsAccountID(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"000000000000", true},
		{"123456789012", true},
		{"999999999999", true},
		{"self", false},
		{"spinifex", false},
		{"", false},
		{"12345678901", false},   // 11 digits
		{"1234567890123", false}, // 13 digits
		{"12345678901a", false},  // non-digit
		{"abcdefghijkl", false},
	}
	for _, tt := range tests {
		got := IsAccountID(tt.input)
		if got != tt.want {
			t.Errorf("IsAccountID(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestGlobalAccountID(t *testing.T) {
	if GlobalAccountID != "000000000000" {
		t.Errorf("GlobalAccountID = %q, want %q", GlobalAccountID, "000000000000")
	}
	if !IsAccountID(GlobalAccountID) {
		t.Error("GlobalAccountID should be a valid account ID")
	}
}
