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

// dumpFlowLines renders n flows under eniID's cookie as `ovs-ofctl --no-stats
// dump-flows` prints them.
func dumpFlowLines(eniID string, n int) string {
	var b strings.Builder
	for range n {
		b.WriteString(" cookie=" + imdsFlowCookie(IMDSEndpointName(eniID)) + ", priority=100,in_port=1 actions=output:3\n")
	}
	return b.String()
}

// withEndpointSysctls points endpointSysctlDir at a temp tree in which each
// listed endpoint has accept_local set to the given value.
func withEndpointSysctls(t *testing.T, acceptLocal map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for endpoint, val := range acceptLocal {
		if err := os.MkdirAll(filepath.Join(dir, endpoint), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, endpoint, "accept_local"), []byte(val+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig := endpointSysctlDir
	endpointSysctlDir = dir
	t.Cleanup(func() { endpointSysctlDir = orig })
}

func TestIMDSDatapathStates(t *testing.T) {
	const (
		intact     = "eni-0intact"
		partial    = "eni-0partial"
		flushed    = "eni-0flushed"
		reloaded   = "eni-0reloaded"
		noEndpoint = "eni-0noendpoint"
		departed   = "eni-0departed"
	)
	dump := "NXST_FLOW reply (xid=0x4):\n" +
		dumpFlowLines(intact, IMDSTapFlowCount) +
		dumpFlowLines(partial, 3) +
		dumpFlowLines(reloaded, IMDSTapFlowCount) +
		dumpFlowLines(noEndpoint, IMDSTapFlowCount) +
		// A guest torn down on this node leaves no running ENI to claim these.
		dumpFlowLines(departed, IMDSTapFlowCount) +
		// A zero-cookie flow is printed without the field.
		" priority=0 actions=drop\n"

	s := newStubRunner()
	s.expect("ovs-ofctl --no-stats dump-flows "+IMDSBridge, []byte(dump), nil)
	withEndpointSysctls(t, map[string]string{
		IMDSEndpointName(intact):   "1",
		IMDSEndpointName(partial):  "1",
		IMDSEndpointName(flushed):  "1",
		IMDSEndpointName(reloaded): "0",
	})

	got, err := IMDSDatapathStates(context.Background(), s, []string{intact, partial, flushed, reloaded, noEndpoint})
	if err != nil {
		t.Fatalf("IMDSDatapathStates: %v", err)
	}
	want := map[string]vm.IMDSDatapathState{
		intact:     {FlowsFound: IMDSTapFlowCount, FlowsExpected: IMDSTapFlowCount, EndpointReady: true},
		partial:    {FlowsFound: 3, FlowsExpected: IMDSTapFlowCount, EndpointReady: true},
		flushed:    {FlowsFound: 0, FlowsExpected: IMDSTapFlowCount, EndpointReady: true},
		reloaded:   {FlowsFound: IMDSTapFlowCount, FlowsExpected: IMDSTapFlowCount, EndpointReady: false},
		noEndpoint: {FlowsFound: IMDSTapFlowCount, FlowsExpected: IMDSTapFlowCount, EndpointReady: false},
	}
	if len(got) != len(want) {
		t.Errorf("got %d states, want %d: %v", len(got), len(want), got)
	}
	for eni, w := range want {
		if got[eni] != w {
			t.Errorf("%s: got %+v, want %+v", eni, got[eni], w)
		}
	}
	if !got[intact].Intact() {
		t.Errorf("a complete datapath must read as intact: %+v", got[intact])
	}
	for _, eni := range []string{partial, flushed, reloaded, noEndpoint} {
		if got[eni].Intact() {
			t.Errorf("%s must not read as intact: %+v", eni, got[eni])
		}
	}
}

// A failed dump must surface, never read as "every ENI lost its flows" and
// trigger a repair of every guest on the node.
func TestIMDSDatapathStates_DumpFailure(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-ofctl", nil, errors.New("br-imds.mgmt: connection refused"))
	if got, err := IMDSDatapathStates(context.Background(), s, []string{"eni-0abc"}); err == nil {
		t.Fatalf("expected the dump failure to surface, got %v", got)
	}
}

func TestCountFlowsByCookie_MalformedCookie(t *testing.T) {
	if _, err := countFlowsByCookie([]byte(" cookie=0xzz, priority=1 actions=drop\n")); err == nil {
		t.Fatal("expected a malformed cookie to be an error")
	}
}

// IMDSTapFlowCount is what the drift check compares against, so it must match
// what an install actually adds. A new flow without a bumped count would make
// every healthy guest read as drifted and get re-installed every pass.
func TestIMDSTapFlowCount_MatchesInstall(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	s.expect("ip", nil, nil)
	s.expect(hostcommand.EndpointSysctlHelper, nil, nil)
	s.expect("ovs-ofctl", nil, nil)

	d := imdsDatapathSpec("eni-0abc1234", "02:00:00:00:01:05", "subnet-0fedcba9")
	if err := installIMDSDatapath(context.Background(), s, d); err != nil {
		t.Fatalf("installIMDSDatapath: %v", err)
	}
	cookie := imdsFlowCookie(d.Endpoint)
	if n := countCalls(s.calls, "ovs-ofctl add-flow "+IMDSBridge+" cookie="+cookie+","); n != IMDSTapFlowCount {
		t.Errorf("install added %d flows under the tap's cookie, IMDSTapFlowCount is %d", n, IMDSTapFlowCount)
	}
}

func TestRepairIMDSDatapath(t *testing.T) {
	const eniID = "eni-0abc1234"
	d := imdsDatapathSpec(eniID, "02:00:00:00:01:05", "subnet-0fedcba9")
	cookie := imdsFlowCookie(d.Endpoint)

	t.Run("tap on br-imds is re-installed", func(t *testing.T) {
		s := newStubRunner()
		s.expect("ovs-vsctl list-ports "+IMDSBridge, []byte(d.Endpoint+"\n"+d.PatchIMDS+"\n"+d.Tap+"\n"), nil)
		s.expect("ovs-vsctl --may-exist", nil, nil)
		s.expect("ovs-vsctl set", nil, nil)
		s.expect("ip", nil, nil)
		s.expect(hostcommand.EndpointSysctlHelper, nil, nil)
		s.expect("ovs-ofctl", nil, nil)

		if err := repairIMDSDatapath(context.Background(), s, d); err != nil {
			t.Fatalf("repairIMDSDatapath: %v", err)
		}
		if n := countCalls(s.calls, "ovs-ofctl add-flow "+IMDSBridge+" cookie="+cookie+","); n != IMDSTapFlowCount {
			t.Errorf("repair added %d flows, want %d", n, IMDSTapFlowCount)
		}
		if !s.called("ip rule add oif " + d.Endpoint) {
			t.Errorf("repair must restore the reply rule a kmod reload drops; calls: %v", s.calls)
		}
	})

	// A tap gone from br-imds belongs to a guest being torn down. Re-creating
	// its patch would leave an imi- port that vpcd binds and OVN claims.
	t.Run("tap gone from br-imds is left alone", func(t *testing.T) {
		s := newStubRunner()
		s.expect("ovs-vsctl list-ports "+IMDSBridge, []byte(d.Endpoint+"\n"), nil)

		err := repairIMDSDatapath(context.Background(), s, d)
		if !errors.Is(err, vm.ErrIMDSTapDetached) {
			t.Fatalf("want ErrIMDSTapDetached, got %v", err)
		}
		if len(s.calls) != 1 {
			t.Errorf("a detached tap must not be touched; calls: %v", s.calls)
		}
	})

	t.Run("port list failure installs nothing", func(t *testing.T) {
		s := newStubRunner()
		s.expect("ovs-vsctl list-ports", nil, errors.New("ovsdb unreachable"))

		err := repairIMDSDatapath(context.Background(), s, d)
		if err == nil || errors.Is(err, vm.ErrIMDSTapDetached) {
			t.Fatalf("want a plain error, got %v", err)
		}
		if len(s.calls) != 1 {
			t.Errorf("nothing may be installed without knowing the tap is attached; calls: %v", s.calls)
		}
	})
}
