package identifiers

import (
	"testing"
)

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

func TestDefaultAccountID(t *testing.T) {
	id := DefaultAccountID()
	if id != "000000000001" {
		t.Errorf("DefaultAccountID() = %q, want %q", id, "000000000001")
	}
	if !IsAccountID(id) {
		t.Errorf("DefaultAccountID() = %q is not a valid account ID", id)
	}
	if id == GlobalAccountID {
		t.Error("DefaultAccountID() must differ from GlobalAccountID")
	}
}

func TestDefaultAccountName(t *testing.T) {
	if got := DefaultAccountName(); got != "spinifex" {
		t.Errorf("DefaultAccountName() = %q, want %q", got, "spinifex")
	}
}
