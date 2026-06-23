package host

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func testDatapath() IMDSTapDatapath {
	return IMDSTapDatapath{
		Tap:         "tapabc123",
		Endpoint:    "ime-12345678",
		EndpointMAC: "02:00:00:00:01:fe",
		GuestMAC:    "02:00:00:00:01:05",
		GatewayMAC:  "02:aa:aa:aa:aa:aa",
		PatchIMDS:   "imp-12345678",
		PatchInt:    "imi-12345678",
		IfaceID:     "port-eni-12345678",
	}
}

func TestIMDSEndpointName(t *testing.T) {
	got := IMDSEndpointName("eni-0abc1234deadbeef")
	if len(got) > 15 {
		t.Errorf("endpoint name %q exceeds IFNAMSIZ-1 (15)", got)
	}
	if !strings.HasPrefix(got, "ime-") {
		t.Errorf("endpoint name %q missing ime- prefix", got)
	}
	// Short ENIs are not truncated.
	if n := IMDSEndpointName("eni-abc"); n != "ime-abc" {
		t.Errorf("short ENI: got %q want ime-abc", n)
	}
}

func TestIMDSPatchPortNames(t *testing.T) {
	const eni = "eni-0abc1234deadbeef"
	imds := IMDSPatchPort(eni)
	intp := IMDSIntPatchPort(eni)
	if len(imds) > 15 || len(intp) > 15 {
		t.Errorf("patch names exceed IFNAMSIZ-1 (15): %q %q", imds, intp)
	}
	if !strings.HasPrefix(imds, "imp-") || !strings.HasPrefix(intp, "imi-") {
		t.Errorf("patch name prefixes wrong: %q %q", imds, intp)
	}
	// Endpoint and both patch ports must be distinct for the same ENI.
	ep := IMDSEndpointName(eni)
	if imds == intp || imds == ep || intp == ep {
		t.Errorf("per-tap port names collide: endpoint=%q patch_imds=%q patch_int=%q", ep, imds, intp)
	}
}

func TestIMDSEndpointMACDeterministic(t *testing.T) {
	a := IMDSEndpointMAC("eni-0abc1234")
	b := IMDSEndpointMAC("eni-0abc1234")
	c := IMDSEndpointMAC("eni-9999")
	if a != b {
		t.Errorf("endpoint MAC not deterministic: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("distinct ENIs share an endpoint MAC: %q", a)
	}
}

func TestIMDSFlowCookiePerTap(t *testing.T) {
	c1 := imdsFlowCookie("ime-11111111")
	c2 := imdsFlowCookie("ime-22222222")
	if c1 == c2 {
		t.Errorf("distinct endpoints share a flow cookie: %q", c1)
	}
	for _, c := range []string{c1, c2} {
		if !strings.HasPrefix(c, imdsCookiePrefix) {
			t.Errorf("cookie %q missing group prefix %q", c, imdsCookiePrefix)
		}
	}
}

func TestInstallTapDatapathValidate(t *testing.T) {
	s := newStubRunner()
	d := testDatapath()
	d.GatewayMAC = ""
	if err := InstallTapDatapath(context.Background(), s, d); err == nil ||
		!strings.Contains(err.Error(), "GatewayMAC") {
		t.Fatalf("expected GatewayMAC validation error, got %v", err)
	}
	if len(s.calls) != 0 {
		t.Errorf("validation must fail before issuing commands; calls: %v", s.calls)
	}
}

func TestInstallTapDatapath(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	s.expect("ip", nil, nil)
	s.expect("sysctl", nil, nil)
	s.expect("ovs-ofctl", nil, nil)

	d := testDatapath()
	if err := InstallTapDatapath(context.Background(), s, d); err != nil {
		t.Fatalf("InstallTapDatapath: %v", err)
	}
	cookie := imdsFlowCookie(d.Endpoint)

	want := []string{
		// Endpoint: internal port, MAC, up, captured addresses, sysctls.
		"ovs-vsctl --may-exist add-port " + IMDSBridge + " " + d.Endpoint + " -- set Interface " + d.Endpoint + " type=internal",
		"ip link set " + d.Endpoint + " address " + d.EndpointMAC,
		"ip link set " + d.Endpoint + " up",
		"ip addr add " + imdsMetaAddr + "/32 dev " + d.Endpoint,
		"ip addr add " + imdsDNSAddr + "/32 dev " + d.Endpoint,
		"sysctl -qw net.ipv4.conf." + d.Endpoint + ".rp_filter=0",
		"sysctl -qw net.ipv4.conf." + d.Endpoint + ".accept_local=1",
		// Flows are not cleared here: installIMDSDatapath clears the shared cookie
		// once up front so this install does not wipe the patch's forward flows.
		// Ingress demux (gateway dst MAC -> endpoint MAC), one per captured addr.
		"ovs-ofctl add-flow " + IMDSBridge + " cookie=" + cookie + ",table=0,priority=200,in_port=" + d.Tap + ",ip,nw_dst=" + imdsMetaAddr + ",actions=mod_dl_dst:" + d.EndpointMAC + ",output:" + d.Endpoint,
		"ovs-ofctl add-flow " + IMDSBridge + " cookie=" + cookie + ",table=0,priority=200,in_port=" + d.Tap + ",ip,nw_dst=" + imdsDNSAddr + ",actions=mod_dl_dst:" + d.EndpointMAC + ",output:" + d.Endpoint,
		// Egress (L2 rewritten to look like the gateway).
		"ovs-ofctl add-flow " + IMDSBridge + " cookie=" + cookie + ",table=0,priority=200,in_port=" + d.Endpoint + ",ip,actions=mod_dl_src:" + d.GatewayMAC + ",mod_dl_dst:" + d.GuestMAC + ",output:" + d.Tap,
	}
	for _, w := range want {
		if !s.called(w) {
			t.Errorf("missing command:\n  %q\ncalls: %v", w, s.calls)
		}
	}

	// InstallTapDatapath must not clear flows: installIMDSDatapath owns the single
	// up-front clear, so a clear here would wipe the patch's forward flows.
	if s.called("ovs-ofctl del-flows") {
		t.Errorf("InstallTapDatapath must not clear flows; calls: %v", s.calls)
	}
}

func TestInstallTapDatapathToleratesExistingAddr(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	s.expect("ip link set", nil, nil)
	s.expect("ip addr add", []byte("RTNETLINK answers: File exists"), errors.New("exit status 2"))
	s.expect("sysctl", nil, nil)
	s.expect("ovs-ofctl", nil, nil)

	if err := InstallTapDatapath(context.Background(), s, testDatapath()); err != nil {
		t.Fatalf("File exists on addr add must be tolerated, got: %v", err)
	}
}

func TestRemoveTapDatapath(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-ofctl", nil, nil)
	s.expect("ovs-vsctl", nil, nil)

	d := testDatapath()
	if err := RemoveTapDatapath(context.Background(), s, d); err != nil {
		t.Fatalf("RemoveTapDatapath: %v", err)
	}
	cookie := imdsFlowCookie(d.Endpoint)
	for _, w := range []string{
		"ovs-ofctl del-flows " + IMDSBridge + " cookie=" + cookie + "/-1",
		"ovs-vsctl --if-exists del-port " + IMDSBridge + " " + d.Endpoint,
	} {
		if !s.called(w) {
			t.Errorf("missing command %q; calls: %v", w, s.calls)
		}
	}
}

// TestRemoveTapDatapathDeletesEndpointDespitePatchError guards against the
// teardown leaking the endpoint when the br-int patch delete fails: every
// del-port must run regardless, and the surfaced error must join both failures.
func TestRemoveTapDatapathDeletesEndpointDespitePatchError(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-ofctl", nil, nil)
	d := testDatapath()
	s.expect("ovs-vsctl --if-exists del-port "+IMDSBridge+" "+d.PatchIMDS, nil, nil)
	s.expect("ovs-vsctl --if-exists del-port br-int "+d.PatchInt, []byte("boom"), errors.New("exit 1"))
	s.expect("ovs-vsctl --if-exists del-port "+IMDSBridge+" "+d.Endpoint, nil, nil)

	err := RemoveTapDatapath(context.Background(), s, d)
	if err == nil {
		t.Fatal("expected error when br-int patch delete fails")
	}
	if !strings.Contains(err.Error(), d.PatchInt) {
		t.Errorf("error must mention failed br-int patch %q, got: %v", d.PatchInt, err)
	}
	// The endpoint delete must still have run, or it leaks on br-imds.
	if !s.called("ovs-vsctl --if-exists del-port " + IMDSBridge + " " + d.Endpoint) {
		t.Errorf("endpoint must be deleted even after a patch delete failure; calls: %v", s.calls)
	}
}
