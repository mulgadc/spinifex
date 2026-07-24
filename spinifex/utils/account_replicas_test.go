package utils

import (
	"testing"
)

func TestDefaultKVReplicas(t *testing.T) {
	t.Cleanup(func() { defaultKVReplicas.Store(0) })

	if got := DefaultKVReplicas(); got != 1 {
		t.Fatalf("unset default = %d, want 1", got)
	}
	SetDefaultKVReplicas(0)
	if got := DefaultKVReplicas(); got != 1 {
		t.Fatalf("SetDefaultKVReplicas(0) = %d, want 1 (clamped)", got)
	}
	SetDefaultKVReplicas(3)
	if got := DefaultKVReplicas(); got != 3 {
		t.Fatalf("SetDefaultKVReplicas(3) = %d, want 3", got)
	}
}
