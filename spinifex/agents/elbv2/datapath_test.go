package elbv2

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testDataMAC = "02:00:00:aa:00:01"
	testMgmtMAC = "02:00:00:aa:00:02"
	testNetcfg  = "NIC0_MAC=" + testDataMAC + "\nNIC0_CIDR=10.0.1.5/24\nNIC0_GW=10.0.1.1\nNIC0_DEFAULT=1\n" +
		"NIC1_MAC=" + testMgmtMAC + "\nNIC1_CIDR=10.255.0.9/24\nNIC1_DEFAULT=0\n"
)

// datapathAgent returns a test agent whose netcfg and sysfs live in a temp dir.
func datapathAgent(t *testing.T, gwURL, netcfg string) *Agent {
	t.Helper()
	a := newTestAgent(t, gwURL)
	dir := t.TempDir()

	a.netcfgPath = filepath.Join(dir, "netcfg")
	if err := os.WriteFile(a.netcfgPath, []byte(netcfg), 0o600); err != nil {
		t.Fatal(err)
	}

	a.sysNetDir = filepath.Join(dir, "net")
	for name, mac := range map[string]string{"ens3": testDataMAC, "ens4": testMgmtMAC} {
		stats := filepath.Join(a.sysNetDir, name, "statistics")
		if err := os.MkdirAll(stats, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(a.sysNetDir, name, "address"), []byte(mac+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for file, v := range map[string]string{"rx_packets": "100", "tx_packets": "200", "rx_dropped": "3", "tx_dropped": "4"} {
			if err := os.WriteFile(filepath.Join(stats, file), []byte(v+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	a.instanceIDFn = func() (string, error) { return "i-0123456789abcdef0", nil }
	return a
}

func TestParseNetcfg_SkipsMgmtNICAndMissingGateway(t *testing.T) {
	blob := testNetcfg +
		"NIC2_MAC=02:00:00:aa:00:03\nNIC2_CIDR=10.0.2.5/24\nNIC2_GW=10.0.2.1\nNIC2_DEFAULT=0\n" +
		"NIC3_MAC=02:00:00:aa:00:04\nNIC3_DHCP=1\nNIC3_DEFAULT=0\n" +
		"NIC4_CIDR=10.0.4.5/24\nNIC4_GW=10.0.4.1\n" +
		"NIC5_MAC=02:00:00:aa:00:06\nNIC5_CIDR=not-a-cidr\nNIC5_GW=10.0.5.1\n"

	got := parseNetcfg([]byte(blob))
	if len(got) != 2 {
		t.Fatalf("got %d NICs, want the 2 with a gateway: %+v", len(got), got)
	}
	if got[0].MAC != testDataMAC || got[0].Gateway != netip.MustParseAddr("10.0.1.1") || got[0].Addr != netip.MustParseAddr("10.0.1.5") {
		t.Errorf("first NIC = %+v", got[0])
	}
	if got[1].MAC != "02:00:00:aa:00:03" || got[1].Gateway != netip.MustParseAddr("10.0.2.1") {
		t.Errorf("second NIC = %+v", got[1])
	}
}

func TestParseNetcfg_OrdersByNICIndexAndLowercasesMAC(t *testing.T) {
	blob := "NIC10_MAC=02:00:00:AA:00:0A\nNIC10_CIDR=10.0.9.5/24\nNIC10_GW=10.0.9.1\n" +
		"NIC2_MAC=02:00:00:AA:00:02\nNIC2_CIDR=10.0.2.5/24\nNIC2_GW=10.0.2.1\n"
	got := parseNetcfg([]byte(blob))
	if len(got) != 2 || got[0].MAC != "02:00:00:aa:00:02" || got[1].MAC != "02:00:00:aa:00:0a" {
		t.Fatalf("order or case wrong: %+v", got)
	}
}

func TestIfaceByMACAndCounters(t *testing.T) {
	a := datapathAgent(t, "http://unused", testNetcfg)

	iface, err := ifaceByMAC(a.sysNetDir, "02:00:00:AA:00:01")
	if err != nil || iface != "ens3" {
		t.Fatalf("iface = %q, err = %v", iface, err)
	}
	if _, err := ifaceByMAC(a.sysNetDir, "02:00:00:ff:ff:ff"); err == nil {
		t.Error("an unknown MAC must not resolve to an interface")
	}

	var r NICReport
	readCounters(a.sysNetDir, iface, &r)
	if r.RxPackets != 100 || r.TxPackets != 200 || r.RxDropped != 3 || r.TxDropped != 4 {
		t.Errorf("counters = %+v", r)
	}
}

func TestARPFrames_RequestShapeAndReplyMatching(t *testing.T) {
	mac, _ := net.ParseMAC(testDataMAC)
	src, gw := netip.MustParseAddr("10.0.1.5"), netip.MustParseAddr("10.0.1.1")

	req := buildARPRequest(mac, src, gw)
	if len(req) != 42 || req[12] != 0x08 || req[13] != 0x06 || req[21] != 1 {
		t.Fatalf("request frame malformed: % x", req)
	}
	if string(req[38:42]) != string(gw.AsSlice()) {
		t.Errorf("target IP not the gateway: % x", req[38:42])
	}

	reply := make([]byte, 42)
	copy(reply[0:6], mac)
	reply[12], reply[13] = 0x08, 0x06
	reply[21] = 2
	copy(reply[28:32], gw.AsSlice())
	copy(reply[32:38], mac)
	if !isARPReplyFrom(reply, mac, gw) {
		t.Error("a reply from the gateway to our MAC must match")
	}
	if isARPReplyFrom(reply, mac, netip.MustParseAddr("10.0.1.99")) {
		t.Error("a reply from another host must not match")
	}
	reply[21] = 1
	if isARPReplyFrom(reply, mac, gw) {
		t.Error("a request must not count as a reply")
	}
	if isARPReplyFrom(reply[:20], mac, gw) {
		t.Error("a short frame must not match")
	}
}

func TestProbeNICs_ProbesOnlyTheGatewayNIC(t *testing.T) {
	a := datapathAgent(t, "http://unused", testNetcfg)
	var mu sync.Mutex
	var probed []string
	a.arpProbeFn = func(iface string, _ net.HardwareAddr, src, gw netip.Addr, _ time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		probed = append(probed, fmt.Sprintf("%s %s>%s", iface, src, gw))
		return nil
	}

	reports := a.probeNICs()
	if len(reports) != 1 || reports[0].MAC != testDataMAC || !reports[0].ProbeOK || reports[0].ConsecutiveFailures != 0 {
		t.Fatalf("reports = %+v", reports)
	}
	if len(probed) != 1 || probed[0] != "ens3 10.0.1.5>10.0.1.1" {
		t.Errorf("probed = %v, want only the data NIC", probed)
	}
	if reports[0].RxPackets != 100 || reports[0].TxDropped != 4 {
		t.Errorf("counters missing from report: %+v", reports[0])
	}
}

func TestProbeNICs_TwoMissesThenSuccessNeverReportsFailing(t *testing.T) {
	a := datapathAgent(t, "http://unused", testNetcfg)
	results := []error{errARPNoReply, errARPNoReply, nil, errARPNoReply, errARPNoReply, nil}
	var n atomic.Int32
	a.arpProbeFn = func(string, net.HardwareAddr, netip.Addr, netip.Addr, time.Duration) error {
		return results[n.Add(1)-1]
	}

	for i, want := range []int64{1, 2, 0, 1, 2, 0} {
		r := a.probeNICs()
		if len(r) != 1 || !r[0].ProbeOK || r[0].ConsecutiveFailures != want {
			t.Fatalf("tick %d: report = %+v, want ok with %d failures", i, r, want)
		}
	}
}

func TestProbeNICs_ThreeMissesReportFailingAndRecoveryClears(t *testing.T) {
	a := datapathAgent(t, "http://unused", testNetcfg)
	var fail atomic.Bool
	fail.Store(true)
	a.arpProbeFn = func(string, net.HardwareAddr, netip.Addr, netip.Addr, time.Duration) error {
		if fail.Load() {
			return errARPNoReply
		}
		return nil
	}

	for i := 1; i <= 4; i++ {
		r := a.probeNICs()
		wantOK := i < 3 // failing from the third consecutive miss
		if r[0].ProbeOK != wantOK || r[0].ConsecutiveFailures != int64(i) {
			t.Fatalf("miss %d: report = %+v, want ProbeOK=%v", i, r[0], wantOK)
		}
	}

	fail.Store(false)
	if r := a.probeNICs(); !r[0].ProbeOK || r[0].ConsecutiveFailures != 0 {
		t.Errorf("a successful probe must clear the failure: %+v", r[0])
	}
}

func TestProbeNICs_BlockedProbeIsBoundedAndCountsAsMiss(t *testing.T) {
	a := datapathAgent(t, "http://unused", testNetcfg)
	a.probeWait = 50 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	a.arpProbeFn = func(string, net.HardwareAddr, netip.Addr, netip.Addr, time.Duration) error {
		calls.Add(1)
		<-release
		return nil
	}

	for i := 1; i <= 3; i++ {
		start := time.Now()
		r := a.probeNICs()
		if d := time.Since(start); d > time.Second {
			t.Fatalf("tick %d waited %s on a stuck probe", i, d)
		}
		if len(r) != 1 || r[0].ConsecutiveFailures != int64(i) {
			t.Fatalf("tick %d: report = %+v, want %d misses", i, r, i)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("a stuck probe was started %d times, want 1 (no goroutine pile-up)", got)
	}
}

func TestTick_BlockedProbeDoesNotDelayHeartbeatBeyondOneTick(t *testing.T) {
	var got atomic.Pointer[url.Values]
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("Action") == "LBAgentHeartbeat" {
			v := r.Form
			got.Store(&v)
		}
		fmt.Fprint(w, `<LBAgentHeartbeatResponse><LBAgentHeartbeatResult><Status>active</Status><ConfigHash>h1</ConfigHash></LBAgentHeartbeatResult></LBAgentHeartbeatResponse>`)
	}))
	defer gw.Close()

	a := datapathAgent(t, gw.URL, testNetcfg)
	a.localConfigHash = "h1"
	a.probeWait = 50 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	a.arpProbeFn = func(string, net.HardwareAddr, netip.Addr, netip.Addr, time.Duration) error {
		<-release
		return nil
	}

	start := time.Now()
	a.tick()
	if d := time.Since(start); d >= pollInterval {
		t.Fatalf("tick took %s with a stuck probe, want under one %s tick", d, pollInterval)
	}
	if got.Load() == nil {
		t.Fatal("the heartbeat was never sent")
	}
}

func TestHeartbeat_EncodesEvidenceWhenPresent(t *testing.T) {
	var got atomic.Pointer[url.Values]
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		v := r.Form
		got.Store(&v)
		fmt.Fprint(w, `<LBAgentHeartbeatResponse><LBAgentHeartbeatResult><Status>active</Status><ConfigHash>h1</ConfigHash></LBAgentHeartbeatResult></LBAgentHeartbeatResponse>`)
	}))
	defer gw.Close()

	a := datapathAgent(t, gw.URL, testNetcfg)
	a.localConfigHash = "h1"
	a.arpProbeFn = func(string, net.HardwareAddr, netip.Addr, netip.Addr, time.Duration) error { return errARPNoReply }

	a.tick()
	v := got.Load()
	if v == nil {
		t.Fatal("no heartbeat received")
	}
	want := map[string]string{
		"InstanceID":                        "i-0123456789abcdef0",
		"NICs.member.1.MAC":                 testDataMAC,
		"NICs.member.1.ProbeOK":             "true",
		"NICs.member.1.ConsecutiveFailures": "1",
		"NICs.member.1.RxPackets":           "100",
		"NICs.member.1.TxPackets":           "200",
		"NICs.member.1.RxDropped":           "3",
		"NICs.member.1.TxDropped":           "4",
	}
	for k, w := range want {
		if g := v.Get(k); g != w {
			t.Errorf("%s = %q, want %q", k, g, w)
		}
	}
	if v.Get("NICs.member.2.MAC") != "" {
		t.Error("the mgmt NIC must not be reported")
	}
}

func TestHeartbeat_OmitsEvidenceWithoutNetcfg(t *testing.T) {
	var got atomic.Pointer[url.Values]
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		v := r.Form
		got.Store(&v)
		fmt.Fprint(w, `<LBAgentHeartbeatResponse><LBAgentHeartbeatResult><Status>active</Status><ConfigHash>h1</ConfigHash></LBAgentHeartbeatResult></LBAgentHeartbeatResponse>`)
	}))
	defer gw.Close()

	a := datapathAgent(t, gw.URL, testNetcfg)
	a.localConfigHash = "h1"
	a.netcfgPath = filepath.Join(t.TempDir(), "absent")
	a.instanceIDFn = func() (string, error) { return "", errors.New("must not be asked without NICs") }

	a.tick()
	v := got.Load()
	if v == nil {
		t.Fatal("no heartbeat received")
	}
	for k := range *v {
		if k == "InstanceID" || len(k) >= 4 && k[:4] == "NICs" {
			t.Errorf("unexpected evidence parameter %s", k)
		}
	}
}

func TestDataPathEvidence_InstanceIDRetriedUntilKnown(t *testing.T) {
	a := datapathAgent(t, "http://unused", testNetcfg)
	a.arpProbeFn = func(string, net.HardwareAddr, netip.Addr, netip.Addr, time.Duration) error { return nil }
	var calls atomic.Int32
	a.instanceIDFn = func() (string, error) {
		if calls.Add(1) < 3 {
			return "", errors.New("metadata not ready")
		}
		return "i-late", nil
	}

	for i, want := range []string{"", "", "i-late", "i-late"} {
		if got := a.dataPathEvidence().InstanceID; got != want {
			t.Fatalf("call %d: InstanceID = %q, want %q", i, got, want)
		}
	}
	if calls.Load() != 3 {
		t.Errorf("metadata asked %d times, want 3 (stop once known)", calls.Load())
	}
}

func TestARPProbe_NoResponderFailsClosed(t *testing.T) {
	mac, _ := net.ParseMAC(testDataMAC)
	// Nobody answers an ARP request sent on loopback, so a probe that returned
	// nil here would mask a dead data path. It fails either on the socket (no
	// CAP_NET_RAW) or on the missing reply.
	err := arpProbe("lo", mac, netip.MustParseAddr("10.9.9.2"), netip.MustParseAddr("10.9.9.1"), 100*time.Millisecond)
	if err == nil {
		t.Fatal("a probe with no responder must fail")
	}
	if err := arpProbe("no-such-iface0", mac, netip.MustParseAddr("10.9.9.2"), netip.MustParseAddr("10.9.9.1"), 100*time.Millisecond); err == nil {
		t.Fatal("a probe on a missing interface must fail")
	}
}

func TestFetchInstanceID(t *testing.T) {
	serve := func(tokenStatus, idStatus int, wantToken string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
				w.WriteHeader(tokenStatus)
				fmt.Fprint(w, "tok123")
			case r.URL.Path == "/latest/meta-data/instance-id":
				if got := r.Header.Get("X-Aws-Ec2-Metadata-Token"); got != wantToken {
					t.Errorf("token header = %q, want %q", got, wantToken)
				}
				w.WriteHeader(idStatus)
				fmt.Fprint(w, " i-abc123 \n")
			default:
				http.NotFound(w, r)
			}
		}))
	}
	saved := imdsBase
	defer func() { imdsBase = saved }()

	for _, tc := range []struct {
		name                  string
		tokenStatus, idStatus int
		wantToken             string
		wantID                string
		wantErr               bool
	}{
		{"token then id", http.StatusOK, http.StatusOK, "tok123", "i-abc123", false},
		{"token refused falls back to v1", http.StatusNotFound, http.StatusOK, "", "i-abc123", false},
		{"id endpoint error", http.StatusOK, http.StatusServiceUnavailable, "tok123", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(tc.tokenStatus, tc.idStatus, tc.wantToken)
			defer srv.Close()
			imdsBase = srv.URL + "/latest"
			id, err := fetchInstanceID()
			if (err != nil) != tc.wantErr || id != tc.wantID {
				t.Fatalf("id = %q, err = %v", id, err)
			}
		})
	}

	imdsBase = "http://127.0.0.1:1/latest"
	if _, err := fetchInstanceID(); err == nil {
		t.Error("an unreachable metadata service must fail")
	}
}
