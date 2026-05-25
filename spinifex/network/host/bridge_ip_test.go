package host

import "testing"

func TestGetBridgeIPv4_Loopback(t *testing.T) {
	// "lo" is always present and has 127.0.0.1
	ip, err := GetBridgeIPv4("lo")
	if err != nil {
		t.Fatalf("GetBridgeIPv4(lo): %v", err)
	}
	if ip != "127.0.0.1" {
		t.Errorf("GetBridgeIPv4(lo) = %q, want 127.0.0.1", ip)
	}
}

func TestGetBridgeIPv4_NonexistentBridge(t *testing.T) {
	ip, err := GetBridgeIPv4("br-nonexistent-test-xyz")
	if err != nil {
		t.Fatalf("expected nil error for absent bridge, got: %v", err)
	}
	if ip != "" {
		t.Errorf("expected empty IP for absent bridge, got %q", ip)
	}
}
