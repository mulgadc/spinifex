package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	hostcommand "github.com/mulgadc/spinifex/spinifex/runtime/host/command"
)

// imdsHost is a Runner that keeps the host state the drift check reads: the
// tap's flows stay in place, and the sysctl helper writes endpointSysctlDir.
// While flowFails is above zero, each ovs-ofctl flow change fails.
type imdsHost struct {
	eniID     string
	flowFails int
	calls     []string
	bundles   []string
}

func (h *imdsHost) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	joined := strings.Join(append([]string{name}, args...), " ")
	h.calls = append(h.calls, joined)
	switch {
	case strings.HasPrefix(joined, "ovs-vsctl list-ports "+IMDSBridge):
		return []byte(vm.TapDeviceName(h.eniID) + "\n"), nil
	case strings.HasPrefix(joined, "ovs-ofctl --no-stats dump-flows "+IMDSBridge):
		return []byte(dumpFlowLines(h.eniID, IMDSTapFlowCount)), nil
	case name == "ovs-ofctl" && h.flowFails > 0:
		h.flowFails--
		return nil, errors.New("OFPBFC_MSG_FAILED")
	case strings.HasPrefix(joined, "ovs-ofctl --bundle add-flows "+IMDSBridge+" "):
		body, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return nil, err
		}
		h.bundles = append(h.bundles, string(body))
	case name == hostcommand.EndpointSysctlHelper:
		dir := filepath.Join(endpointSysctlDir, args[0])
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(filepath.Join(dir, args[1]), []byte(args[2]+"\n"), 0o644)
	}
	return nil, nil
}

func (h *imdsHost) flowMutations() []string {
	var out []string
	for _, c := range h.calls {
		if strings.HasPrefix(c, "ovs-ofctl ") && !strings.HasPrefix(c, "ovs-ofctl --no-stats dump-flows") {
			out = append(out, c)
		}
	}
	return out
}

func (h *imdsHost) ran(prefix string) bool { return cmdIndex(h.calls, prefix) >= 0 }

// A repair re-installs a running guest's flows. Only one atomic bundle keeps
// the guest's traffic flowing on a fail-mode=secure bridge during the change.
func TestRepairIMDSDatapath_OneAtomicFlowMutation(t *testing.T) {
	const eniID = "eni-0abc1234"
	withEndpointSysctls(t, nil)
	d := imdsDatapathSpec(eniID, "02:00:00:00:01:05", "subnet-0fedcba9")
	h := &imdsHost{eniID: eniID}

	if err := repairIMDSDatapath(context.Background(), h, d); err != nil {
		t.Fatalf("repairIMDSDatapath: %v", err)
	}
	muts := h.flowMutations()
	if len(muts) != 1 || !strings.HasPrefix(muts[0], "ovs-ofctl --bundle add-flows "+IMDSBridge+" ") {
		t.Errorf("want exactly one ovs-ofctl --bundle add-flows, got %d flow changes: %v", len(muts), muts)
	}
}

// The endpoint was re-created and its reply routing is gone, but its 7 flows
// survive. A failed flow install must leave the marker unset, so the drift
// check reads the datapath as broken and the next repair finishes it.
func TestInstallIMDSDatapath_FailedBundleLeavesRepairPending(t *testing.T) {
	const eniID = "eni-0abc1234"
	withEndpointSysctls(t, nil)
	d := imdsDatapathSpec(eniID, "02:00:00:00:01:05", "subnet-0fedcba9")
	h := &imdsHost{eniID: eniID, flowFails: 1}
	marker := hostcommand.EndpointSysctlHelper + " " + d.Endpoint + " accept_local 1"

	err := installIMDSDatapath(context.Background(), h, d)
	if !errors.Is(err, vm.ErrIMDSServingDegraded) {
		t.Fatalf("want ErrIMDSServingDegraded, got %v", err)
	}
	if h.ran(marker) {
		t.Errorf("accept_local set although the install failed; calls: %v", h.calls)
	}
	states, err := IMDSDatapathStates(context.Background(), h, []string{eniID})
	if err != nil {
		t.Fatalf("IMDSDatapathStates: %v", err)
	}
	if states[eniID].Intact() {
		t.Fatalf("a failed install reads as intact, so no pass repairs it: %+v", states[eniID])
	}

	h.calls = nil
	if err := repairIMDSDatapath(context.Background(), h, d); err != nil {
		t.Fatalf("repairIMDSDatapath: %v", err)
	}
	for _, w := range []string{
		"ip route replace default via " + imdsReplyNexthop + " dev " + d.Endpoint,
		"ip neigh replace " + imdsReplyNexthop,
		"ip rule del oif " + d.Endpoint,
		"ip rule add oif " + d.Endpoint,
		marker,
	} {
		if !h.ran(w) {
			t.Errorf("repair did not run %q; calls: %v", w, h.calls)
		}
	}
	states, err = IMDSDatapathStates(context.Background(), h, []string{eniID})
	if err != nil {
		t.Fatalf("IMDSDatapathStates: %v", err)
	}
	if !states[eniID].Intact() {
		t.Errorf("the datapath must read as intact after the repair: %+v", states[eniID])
	}
}

// The marker the drift check reads goes last, after the flows and the reply
// routing it stands for.
func TestInstallIMDSDatapath_MarkerIsLast(t *testing.T) {
	const eniID = "eni-0abc1234"
	withEndpointSysctls(t, nil)
	d := imdsDatapathSpec(eniID, "02:00:00:00:01:05", "subnet-0fedcba9")
	h := &imdsHost{eniID: eniID}

	if err := installIMDSDatapath(context.Background(), h, d); err != nil {
		t.Fatalf("installIMDSDatapath: %v", err)
	}
	last := h.calls[len(h.calls)-1]
	if last != hostcommand.EndpointSysctlHelper+" "+d.Endpoint+" accept_local 1" {
		t.Errorf("last call = %q, want the accept_local marker; calls: %v", last, h.calls)
	}
}

// The bundle drops the cookie's old flows and adds all 7 in one transaction.
func TestInstallIMDSDatapath_BundleReplacesTheCookie(t *testing.T) {
	const eniID = "eni-0abc1234"
	withEndpointSysctls(t, nil)
	d := imdsDatapathSpec(eniID, "02:00:00:00:01:05", "subnet-0fedcba9")
	cookie := imdsFlowCookie(d.Endpoint)
	h := &imdsHost{eniID: eniID}

	if err := installIMDSDatapath(context.Background(), h, d); err != nil {
		t.Fatalf("installIMDSDatapath: %v", err)
	}
	if len(h.bundles) != 1 {
		t.Fatalf("want 1 bundle, got %d", len(h.bundles))
	}
	lines := strings.Split(strings.TrimSpace(h.bundles[0]), "\n")
	if lines[0] != "delete cookie="+cookie+"/-1" {
		t.Errorf("first bundle line = %q, want the cookie delete", lines[0])
	}
	adds := lines[1:]
	if len(adds) != IMDSTapFlowCount {
		t.Errorf("bundle adds %d flows, IMDSTapFlowCount is %d: %v", len(adds), IMDSTapFlowCount, adds)
	}
	for _, l := range adds {
		if !strings.HasPrefix(l, "add cookie="+cookie+",table=0,") {
			t.Errorf("bundle line %q is not an add under the tap's cookie", l)
		}
	}
	forward := "add cookie=" + cookie + ",table=0,priority=100,in_port=" + d.Tap + ",actions=output:" + d.PatchIMDS
	if !strings.Contains(h.bundles[0], forward+"\n") {
		t.Errorf("bundle lacks the tap->patch forward flow %q:\n%s", forward, h.bundles[0])
	}
}

// The launch ensures br-imds before SetupTap, so the install does not repeat
// it. A repair can follow an ovs-vswitchd restart, so it ensures the bridge.
func TestIMDSDatapath_BridgeEnsuredOnlyByRepair(t *testing.T) {
	const eniID = "eni-0abc1234"
	withEndpointSysctls(t, nil)
	d := imdsDatapathSpec(eniID, "02:00:00:00:01:05", "subnet-0fedcba9")
	addBr := "ovs-vsctl --may-exist add-br " + IMDSBridge

	launch := &imdsHost{eniID: eniID}
	if err := installIMDSDatapath(context.Background(), launch, d); err != nil {
		t.Fatalf("installIMDSDatapath: %v", err)
	}
	if launch.ran(addBr) {
		t.Errorf("launch install ensured the bridge again; calls: %v", launch.calls)
	}

	repair := &imdsHost{eniID: eniID}
	if err := repairIMDSDatapath(context.Background(), repair, d); err != nil {
		t.Fatalf("repairIMDSDatapath: %v", err)
	}
	if !repair.ran(addBr) {
		t.Errorf("repair did not ensure the bridge; calls: %v", repair.calls)
	}
}
