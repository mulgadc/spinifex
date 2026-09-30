package viperblockd

import "testing"

func TestNBDTransportValues(t *testing.T) {
	if NBDTransportSocket != "socket" || NBDTransportTCP != "tcp" {
		t.Fatal("unexpected NBD transport values")
	}
}
