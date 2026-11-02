package host

import (
	"context"
	"errors"
	"testing"
)

const (
	flowRestoreGet = "ovs-vsctl --if-exists get Open_vSwitch . other_config:flow-restore-wait"
	upcallShow     = "ovs-appctl upcall/show"
)

// Captured from a node whose ovs-vswitchd came up after a hard power-off and
// never left flow-restore-wait.
const upcallShowHeld = `system@ovs-system:
  flows         : (current 0) (avg 0) (max 0) (limit 10000)
  dump duration : 0ms
  ufid enabled : true

`

// Captured from a healthy node of the same cluster.
const upcallShowRunning = `system@ovs-system:
  flows         : (current 8) (avg 8) (max 75) (limit 50000)
  dump duration : 1ms
  ufid enabled : true

  19: (keys 3)
  20: (keys 0)
`

func TestVswitchdHeldInFlowRestore(t *testing.T) {
	cases := []struct {
		name    string
		flag    string
		upcalls string
		want    bool
	}{
		{"flag set and no upcall threads", `"true"`, upcallShowHeld, true},
		// ovn-controller re-sets the flag on every healthy node after clearing it.
		{"flag set on a node that is forwarding", `"true"`, upcallShowRunning, false},
		{"flag cleared", `"false"`, upcallShowHeld, false},
		{"flag absent", "", upcallShowHeld, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &scriptedRunner{responses: map[string][]byte{
				flowRestoreGet: []byte(tc.flag + "\n"),
				upcallShow:     []byte(tc.upcalls),
			}}
			got, err := VswitchdHeldInFlowRestore(context.Background(), r)
			if err != nil {
				t.Fatalf("VswitchdHeldInFlowRestore: %v", err)
			}
			if got != tc.want {
				t.Fatalf("held = %v, want %v", got, tc.want)
			}
		})
	}
}

// A probe that cannot see the threads must not report the datapath held, or
// a flaky appctl would have the watchdog rewrite the flag on a healthy node.
func TestVswitchdHeldInFlowRestore_UnreadableUpcallsIsAnError(t *testing.T) {
	r := &scriptedRunner{
		responses: map[string][]byte{flowRestoreGet: []byte(`"true"`)},
		errors:    map[string]error{upcallShow: errors.New("no ovs-vswitchd")},
	}
	held, err := VswitchdHeldInFlowRestore(context.Background(), r)
	if err == nil || held {
		t.Fatalf("held = %v, err = %v; want false and an error", held, err)
	}
}

func TestReleaseFlowRestoreWait(t *testing.T) {
	r := &scriptedRunner{}
	if err := ReleaseFlowRestoreWait(context.Background(), r); err != nil {
		t.Fatalf("ReleaseFlowRestoreWait: %v", err)
	}
	want := "ovs-vsctl set Open_vSwitch . other_config:flow-restore-wait=false"
	if calls := r.callArgs(want); len(calls) != 1 {
		t.Fatalf("want one %q, got %v", want, r.calls)
	}
}
