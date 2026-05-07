package autoinstall

import (
	"os"
	"testing"
)

func TestLoadReturnsNilWhenNotAuto(t *testing.T) {
	os.Unsetenv("SPINIFEX_AUTO")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil config when SPINIFEX_AUTO is not set")
	}
}
