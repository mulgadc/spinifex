package host

import (
	"context"
	"fmt"
	"testing"
)

type ovsIface = struct {
	name string
	ids  map[string]string
}

// A node plumbs a distributed EIP only for the instances it is running, and
// the iface-id on the tap is how it knows. Matching must be exact: "port-eni-1"
// and "port-eni-10" are different instances, and a prefix match would make one
// node claim the other's address.
func TestHasLocalPort(t *testing.T) {
	rows := []ovsIface{
		{name: "tap-eni-1", ids: map[string]string{"iface-id": "port-eni-1", "attached-mac": "52:54:00:aa:bb:cc"}},
		{name: "tap-eni-10", ids: map[string]string{"iface-id": "port-eni-10"}},
		{name: "br-int", ids: nil},
	}
	for _, tc := range []struct {
		name string
		port string
		want bool
	}{
		{"bound here", "port-eni-1", true},
		{"a longer id sharing the prefix", "port-eni-10", true},
		{"bound on another chassis", "port-eni-2", false},
		{"prefix of a local id is not a match", "port-eni-", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStubRunner()
			r.expect(listInterfacesCmd, ovsListJSON(t, rows), nil)
			got, err := HasLocalPort(context.Background(), r, tc.port)
			if err != nil {
				t.Fatalf("HasLocalPort: %v", err)
			}
			if got != tc.want {
				t.Errorf("HasLocalPort(%q) = %v, want %v", tc.port, got, tc.want)
			}
		})
	}
}

// A failed query must not read as "not mine". That answer makes the node drop
// an EIP it is in fact hosting, and the next pass would do it again.
func TestHasLocalPortReportsQueryFailure(t *testing.T) {
	r := newStubRunner()
	r.expect(listInterfacesCmd, []byte("ovs-vsctl: database connection failed"), fmt.Errorf("exit 1"))
	if _, err := HasLocalPort(context.Background(), r, "port-eni-1"); err == nil {
		t.Error("a failed OVS query must surface as an error, not as a false verdict")
	}
}

func TestHasLocalPortRequiresAPort(t *testing.T) {
	r := newStubRunner()
	if _, err := HasLocalPort(context.Background(), r, ""); err == nil {
		t.Error("empty port name: expected error")
	}
	if len(r.calls) != 0 {
		t.Errorf("validation failure must not query OVS: %v", r.calls)
	}
}
