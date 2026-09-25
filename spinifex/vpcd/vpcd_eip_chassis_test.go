//test:in-package — skipForeignEIP is unexported and is the gate that keeps
// one node from claiming another node's EIP.

package vpcd

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/policy"
)

// ovsIfaceRunner answers the one `ovs-vsctl list Interface` query the chassis
// check makes, and records that it was asked.
type ovsIfaceRunner struct {
	ifaceIDs []string
	err      error
	calls    int
}

func (r *ovsIfaceRunner) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	r.calls++
	if r.err != nil {
		return []byte("ovs-vsctl: database connection failed"), r.err
	}
	data := make([]any, 0, len(r.ifaceIDs))
	for i, id := range r.ifaceIDs {
		data = append(data, []any{
			fmt.Sprintf("tap%d", i),
			[]any{"map", [][]string{{"iface-id", id}}},
		})
	}
	return json.Marshal(map[string]any{
		"headings": []string{"name", "external_ids"},
		"data":     data,
	})
}

// A distributed EIP is reachable only through the chassis its ENI is bound to.
// Every node runs vpcd and every node sees every EIP, so without this gate all
// three nodes claimed all three addresses — measured on the OCI cluster, and
// the reason two of the three guests were unreachable.
func TestSkipForeignEIP(t *testing.T) {
	distributed := policy.EIPSpec{
		VPCID: "vpc-1", ExternalIP: "192.0.2.10", LogicalIP: "10.0.1.5",
		PortName: "port-eni-1", MAC: "52:54:00:aa:bb:cc",
	}
	for _, tc := range []struct {
		name      string
		eip       policy.EIPSpec
		local     []string
		wantSkip  bool
		wantAsked bool
	}{
		{
			name:      "the instance is on this node",
			eip:       distributed,
			local:     []string{"port-eni-9", "port-eni-1"},
			wantSkip:  false,
			wantAsked: true,
		},
		{
			name:      "the instance is on another node",
			eip:       distributed,
			local:     []string{"port-eni-9"},
			wantSkip:  true,
			wantAsked: true,
		},
		{
			// A centralised EIP hairpins through the gateway chassis, so every
			// node plumbs it exactly as before. Gating it would break the
			// pre-distribution shape during an upgrade.
			name:      "a centralised EIP is never skipped",
			eip:       policy.EIPSpec{VPCID: "vpc-1", ExternalIP: "192.0.2.10", PortName: "port-eni-1"},
			local:     []string{"port-eni-9"},
			wantSkip:  false,
			wantAsked: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &ovsIfaceRunner{ifaceIDs: tc.local}
			skip, err := skipForeignEIP(context.Background(), r, tc.eip)
			if err != nil {
				t.Fatalf("skipForeignEIP: %v", err)
			}
			if skip != tc.wantSkip {
				t.Errorf("skip = %v, want %v", skip, tc.wantSkip)
			}
			if asked := r.calls > 0; asked != tc.wantAsked {
				t.Errorf("queried OVS = %v, want %v", asked, tc.wantAsked)
			}
		})
	}
}

// A failed OVS query must not read as "not mine". That verdict drops the host
// plumbing for an EIP this node is in fact serving, and the next pass would
// reach the same wrong answer — so the error has to reach reconcile's retry.
func TestSkipForeignEIPSurfacesQueryFailure(t *testing.T) {
	r := &ovsIfaceRunner{err: fmt.Errorf("exit 1")}
	_, err := skipForeignEIP(context.Background(), r, policy.EIPSpec{
		ExternalIP: "192.0.2.10", PortName: "port-eni-1", MAC: "52:54:00:aa:bb:cc",
	})
	if err == nil {
		t.Error("a failed OVS query must surface, not silently skip the bind")
	}
}
