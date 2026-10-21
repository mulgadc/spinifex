package hostnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeHost models kernel links, addresses and routes, records every change,
// and can make links appear after a number of polls.
type fakeHost struct {
	links   map[string]*Link
	addrs   map[string][]string
	routes  map[string][]RouteEntry
	changes []string
	// appearAfter makes a link exist only from the nth Link lookup of it.
	appearAfter map[string]int
	lookups     map[string]int
	pending     map[string]*Link
	failOn      string
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		links:       map[string]*Link{},
		addrs:       map[string][]string{},
		routes:      map[string][]RouteEntry{},
		appearAfter: map[string]int{},
		lookups:     map[string]int{},
		pending:     map[string]*Link{},
	}
}

func (f *fakeHost) add(l Link) *fakeHost {
	f.links[l.Name] = &l
	return f
}

func (f *fakeHost) later(l Link, lookups int) *fakeHost {
	f.pending[l.Name] = &l
	f.appearAfter[l.Name] = lookups
	return f
}

func (f *fakeHost) change(format string, args ...any) error {
	c := fmt.Sprintf(format, args...)
	if f.failOn != "" && strings.HasPrefix(c, f.failOn) {
		return errors.New("injected failure")
	}
	f.changes = append(f.changes, c)
	return nil
}

func (f *fakeHost) Link(_ context.Context, name string) (*Link, error) {
	f.lookups[name]++
	if p, ok := f.pending[name]; ok && f.lookups[name] >= f.appearAfter[name] {
		f.links[name] = p
		delete(f.pending, name)
	}
	l, ok := f.links[name]
	if !ok {
		return nil, nil
	}
	c := *l
	return &c, nil
}

func (f *fakeHost) AddVethPair(_ context.Context, name, peer string) error {
	if err := f.change("add-veth %s %s", name, peer); err != nil {
		return err
	}
	f.links[name] = &Link{Name: name, Kind: "veth", Peer: peer, MAC: "aa:aa:aa:aa:aa:01"}
	f.links[peer] = &Link{Name: peer, Kind: "veth", Peer: name, MAC: "aa:aa:aa:aa:aa:02"}
	return nil
}

func (f *fakeHost) SetUp(_ context.Context, name string) error {
	if err := f.change("up %s", name); err != nil {
		return err
	}
	f.links[name].Up = true
	return nil
}

func (f *fakeHost) SetMaster(_ context.Context, name, master string) error {
	if err := f.change("master %s %s", name, master); err != nil {
		return err
	}
	f.links[name].Master = master
	return nil
}

func (f *fakeHost) SetMAC(_ context.Context, name, mac string) error {
	if err := f.change("mac %s %s", name, mac); err != nil {
		return err
	}
	f.links[name].MAC = mac
	return nil
}

func (f *fakeHost) IPv4Addrs(_ context.Context, name string) ([]string, error) {
	return slices.Clone(f.addrs[name]), nil
}

func (f *fakeHost) AddAddr(_ context.Context, name, cidr string) error {
	if err := f.change("addr-add %s %s", name, cidr); err != nil {
		return err
	}
	f.addrs[name] = append(f.addrs[name], cidr)
	return nil
}

func (f *fakeHost) DelAddr(_ context.Context, name, cidr string) error {
	if err := f.change("addr-del %s %s", name, cidr); err != nil {
		return err
	}
	f.addrs[name] = slices.DeleteFunc(f.addrs[name], func(a string) bool { return a == cidr })
	return nil
}

func (f *fakeHost) Routes(_ context.Context, dst string) ([]RouteEntry, error) {
	return slices.Clone(f.routes[dst]), nil
}

func (f *fakeHost) ReplaceRoute(_ context.Context, r Route) error {
	if err := f.change("route %s %s %s %d", r.Dst, r.Dev, r.Src, r.Metric); err != nil {
		return err
	}
	f.routes[r.Dst] = slices.DeleteFunc(f.routes[r.Dst], func(e RouteEntry) bool { return e.Metric == r.Metric })
	f.routes[r.Dst] = append(f.routes[r.Dst], RouteEntry{Dev: r.Dev, Src: r.Src, Metric: r.Metric})
	return nil
}

var _ Host = (*fakeHost)(nil)

// testEnv is a temp root with a fake clock that Sleep advances.
type testEnv struct {
	Env

	host  *fakeHost
	now   time.Time
	slept time.Duration
	// remote is what OVNRemote returns; remoteErr fails it.
	remote    string
	remoteErr error
}

func newTestEnv(t *testing.T, h *fakeHost) *testEnv {
	t.Helper()
	root := t.TempDir()
	te := &testEnv{host: h, now: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}
	te.Env = Env{
		Paths:     PathsAt(root),
		Host:      h,
		LinkWait:  DefaultLinkWait,
		LockWait:  DefaultLockWait,
		OVNRemote: func(context.Context) (string, error) { return te.remote, te.remoteErr },
		Sleep: func(d time.Duration) {
			te.now = te.now.Add(d)
			te.slept += d
		},
		Now: func() time.Time { return te.now },
	}
	for _, dir := range []string{filepath.Dir(te.Paths.Backend), filepath.Dir(te.Paths.Marker), te.Paths.NetworkdDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return te
}

func (te *testEnv) write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (te *testEnv) hostnetBackend(t *testing.T, state string) {
	t.Helper()
	te.write(t, te.Paths.Backend, "hostnet\n")
	te.write(t, te.Paths.State, state)
}

func (te *testEnv) markerExists() bool {
	_, err := os.Stat(te.Paths.Marker)
	return err == nil
}

const vethState = `{
  "version": 1,
  "pending": false,
  "int_bridge": "br-int",
  "wan": {"mode": "veth", "ovs_bridge": "br-ext", "linux_bridge": "br-wan", "host_end": "veth-wan-br", "ovs_end": "veth-wan-ovs"},
  "mgmt": {"bridge": "br-mgmt", "cidr": "10.15.8.1/24", "nic": "mgmt"},
  "encap_route": {"dst": "10.1.0.0/16", "dev": "br-vpc", "src": "10.1.0.5", "metric": 50}
}`

const natState = `{
  "version": 1,
  "pending": false,
  "int_bridge": "br-int",
  "wan": {"mode": "nat", "ovs_bridge": "br-ext", "host_end": "spx-nat-host", "ovs_end": "spx-nat-ovs", "host_mac": "02:00:64:7f:00:01", "host_cidr": "100.127.0.1/24"}
}`

const directState = `{
  "version": 1,
  "pending": false,
  "int_bridge": "br-int",
  "wan": {"mode": "direct", "ovs_bridge": "br-ext", "nic": "eth1"}
}`

// rebootedVethHost is a veth-mode node just after a reboot: OVS and
// NetworkManager have brought back their own links, nothing else.
func rebootedVethHost() *fakeHost {
	h := newFakeHost().
		add(Link{Name: "br-int", Kind: "openvswitch"}).
		add(Link{Name: "br-ext", Kind: "openvswitch"}).
		add(Link{Name: "br-mgmt", Kind: "openvswitch"}).
		add(Link{Name: "br-wan", Kind: "bridge", Up: true}).
		add(Link{Name: "br-vpc", Kind: "bridge", Up: true}).
		add(Link{Name: "mgmt", Up: false})
	h.addrs["br-vpc"] = []string{"10.1.0.5/16"}
	h.addrs["mgmt"] = []string{"169.254.3.4/16"}
	return h
}

func TestApplyRebuildsVethModeAfterReboot(t *testing.T) {
	h := rebootedVethHost()
	te := newTestEnv(t, h)
	te.hostnetBackend(t, vethState)

	if err := Apply(context.Background(), te.Env); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []string{
		"up br-int",
		"up br-ext",
		"add-veth veth-wan-br veth-wan-ovs",
		"master veth-wan-br br-wan",
		"up veth-wan-br",
		"up veth-wan-ovs",
		"up br-mgmt",
		"addr-add br-mgmt 10.15.8.1/24",
		"up mgmt",
		"addr-del mgmt 169.254.3.4/16",
		"route 10.1.0.0/16 br-vpc 10.1.0.5 50",
	}
	if !slices.Equal(h.changes, want) {
		t.Errorf("changes:\n got %q\nwant %q", h.changes, want)
	}
	if !te.markerExists() {
		t.Error("marker not written after a successful apply")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	for name, tc := range map[string]struct {
		state string
		host  func() *fakeHost
	}{
		"veth": {vethState, rebootedVethHost},
		"nat": {natState, func() *fakeHost {
			return newFakeHost().add(Link{Name: "br-int", Kind: "openvswitch"}).add(Link{Name: "br-ext", Kind: "openvswitch"})
		}},
		"direct": {directState, func() *fakeHost {
			return newFakeHost().add(Link{Name: "br-int", Kind: "openvswitch"}).add(Link{Name: "br-ext", Kind: "openvswitch"}).add(Link{Name: "eth1"})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			h := tc.host()
			te := newTestEnv(t, h)
			te.hostnetBackend(t, tc.state)
			if err := Apply(context.Background(), te.Env); err != nil {
				t.Fatalf("first Apply: %v", err)
			}
			h.changes = nil
			if err := Apply(context.Background(), te.Env); err != nil {
				t.Fatalf("second Apply: %v", err)
			}
			if len(h.changes) != 0 {
				t.Errorf("second Apply changed the host: %q", h.changes)
			}
		})
	}
}

func TestApplyNATSetsFixedMACAndTransitAddress(t *testing.T) {
	h := newFakeHost().
		add(Link{Name: "br-int", Kind: "openvswitch", Up: true}).
		add(Link{Name: "br-ext", Kind: "openvswitch", Up: true}).
		add(Link{Name: "spx-nat-host", Kind: "veth", Peer: "spx-nat-ovs", MAC: "be:ef:00:00:00:01"}).
		add(Link{Name: "spx-nat-ovs", Kind: "veth", Peer: "spx-nat-host", Up: true})
	te := newTestEnv(t, h)
	te.hostnetBackend(t, natState)

	if err := Apply(context.Background(), te.Env); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []string{
		"mac spx-nat-host 02:00:64:7f:00:01",
		"addr-add spx-nat-host 100.127.0.1/24",
		"up spx-nat-host",
	}
	if !slices.Equal(h.changes, want) {
		t.Errorf("changes:\n got %q\nwant %q", h.changes, want)
	}
}

func TestApplyMovesVethBridgeEndToRecordedBridge(t *testing.T) {
	h := rebootedVethHost().
		add(Link{Name: "veth-wan-br", Kind: "veth", Peer: "veth-wan-ovs", Master: "br-old", Up: true}).
		add(Link{Name: "veth-wan-ovs", Kind: "veth", Peer: "veth-wan-br", Up: true})
	te := newTestEnv(t, h)
	te.hostnetBackend(t, vethState)

	if err := Apply(context.Background(), te.Env); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(h.changes, "master veth-wan-br br-wan") {
		t.Errorf("veth bridge end not moved to br-wan: %q", h.changes)
	}
	if slices.ContainsFunc(h.changes, func(c string) bool { return strings.HasPrefix(c, "add-veth") }) {
		t.Errorf("recreated a veth pair that already existed: %q", h.changes)
	}
}

func TestApplyReplacesEncapRouteOnOtherDevice(t *testing.T) {
	h := rebootedVethHost()
	h.routes["10.1.0.0/16"] = []RouteEntry{
		{Dev: "br-vpc", Src: "10.1.0.5", Metric: 0},
		{Dev: "br-wan", Src: "10.1.0.5", Metric: 50},
	}
	te := newTestEnv(t, h)
	te.hostnetBackend(t, vethState)

	if err := Apply(context.Background(), te.Env); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !slices.Contains(h.changes, "route 10.1.0.0/16 br-vpc 10.1.0.5 50") {
		t.Errorf("encap route on the wrong device not replaced: %q", h.changes)
	}
}

func TestApplyRefusesForeignLinks(t *testing.T) {
	for name, tc := range map[string]struct {
		links []Link
		want  string
	}{
		"non-veth under the bridge end's name": {
			[]Link{{Name: "veth-wan-br", Kind: "dummy"}},
			"veth-wan-br: it is a dummy link",
		},
		"physical NIC under the OVS end's name": {
			[]Link{{Name: "veth-wan-ovs"}},
			"veth-wan-ovs: it is a physical link",
		},
		"ends peered elsewhere": {
			[]Link{
				{Name: "veth-wan-br", Kind: "veth", Peer: "other0"},
				{Name: "veth-wan-ovs", Kind: "veth", Peer: "other1"},
			},
			`veth-wan-br is peered with "other0" and veth-wan-ovs with "other1"`,
		},
		"one end alone": {
			[]Link{{Name: "veth-wan-ovs", Kind: "veth", Peer: "x"}},
			"veth-wan-br is missing",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := rebootedVethHost()
			for _, l := range tc.links {
				h.add(l)
			}
			te := newTestEnv(t, h)
			te.hostnetBackend(t, vethState)

			err := Apply(context.Background(), te.Env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Apply error = %v, want it to contain %q", err, tc.want)
			}
			if te.markerExists() {
				t.Error("marker written after a refused apply")
			}
			if slices.ContainsFunc(h.changes, func(c string) bool { return strings.Contains(c, "veth-wan") }) {
				t.Errorf("touched a refused link: %q", h.changes)
			}
		})
	}
}

func TestApplyWaitsForLinksOthersCreate(t *testing.T) {
	h := rebootedVethHost()
	delete(h.links, "br-wan")
	h.later(Link{Name: "br-wan", Kind: "bridge", Up: true}, 5)
	te := newTestEnv(t, h)
	te.hostnetBackend(t, vethState)

	if err := Apply(context.Background(), te.Env); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if te.slept == 0 {
		t.Error("Apply did not wait for br-wan")
	}
}

func TestApplyFailsNamingLinksStillMissing(t *testing.T) {
	h := rebootedVethHost()
	delete(h.links, "br-wan")
	delete(h.links, "br-mgmt")
	te := newTestEnv(t, h)
	te.hostnetBackend(t, vethState)

	err := Apply(context.Background(), te.Env)
	if err == nil || !strings.Contains(err.Error(), "still missing after 1m0s: br-mgmt, br-wan") {
		t.Fatalf("Apply error = %v, want it to name br-mgmt and br-wan", err)
	}
	if te.slept < DefaultLinkWait {
		t.Errorf("gave up after %s, before the %s wait", te.slept, DefaultLinkWait)
	}
	if len(h.changes) != 0 {
		t.Errorf("changed the host before every link was there: %q", h.changes)
	}
}

func TestApplyWaitsOnlyForLinksTheModeNeeds(t *testing.T) {
	// A NAT node has no Linux uplink bridge and no mgmt NIC: their absence
	// must not cost a wait.
	h := newFakeHost().
		add(Link{Name: "br-int", Kind: "openvswitch"}).
		add(Link{Name: "br-ext", Kind: "openvswitch"})
	te := newTestEnv(t, h)
	te.hostnetBackend(t, natState)

	if err := Apply(context.Background(), te.Env); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if te.slept != 0 {
		t.Errorf("waited %s on a NAT node", te.slept)
	}
	for name := range h.lookups {
		if name == "br-wan" || name == "mgmt" {
			t.Errorf("looked up %s, which a NAT node does not have", name)
		}
	}
}

func TestApplyWaitsForEncapSourceAddress(t *testing.T) {
	h := rebootedVethHost()
	h.addrs["br-vpc"] = nil
	te := newTestEnv(t, h)
	te.hostnetBackend(t, vethState)

	err := Apply(context.Background(), te.Env)
	if err == nil || !strings.Contains(err.Error(), "10.1.0.5 on br-vpc") {
		t.Fatalf("Apply error = %v, want it to name the missing encap source", err)
	}
}

func TestApplyLeavesUnnamedLinksAlone(t *testing.T) {
	h := rebootedVethHost().add(Link{Name: "wg0", Kind: "wireguard"})
	h.addrs["wg0"] = []string{"10.99.0.1/24"}
	h.addrs["br-wan"] = []string{"192.168.0.13/24"}
	te := newTestEnv(t, h)
	te.hostnetBackend(t, vethState)

	if err := Apply(context.Background(), te.Env); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, c := range h.changes {
		if strings.Contains(c, "wg0") || strings.HasPrefix(c, "addr-del br-wan") {
			t.Errorf("touched something the state file does not name: %q", c)
		}
	}
	if h.links["wg0"] == nil || len(h.addrs["wg0"]) != 1 || len(h.addrs["br-wan"]) != 1 {
		t.Error("an operator link or the uplink address changed")
	}
}

func TestApplyFailureRemovesMarker(t *testing.T) {
	h := rebootedVethHost()
	te := newTestEnv(t, h)
	te.hostnetBackend(t, vethState)
	if err := Apply(context.Background(), te.Env); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := Check(context.Background(), te.Env); err != nil {
		t.Fatalf("Check after a successful apply: %v", err)
	}

	delete(h.links, "veth-wan-br")
	delete(h.links, "veth-wan-ovs")
	h.failOn = "add-veth"
	if err := Apply(context.Background(), te.Env); err == nil {
		t.Fatal("second Apply succeeded despite the injected failure")
	}
	if te.markerExists() {
		t.Error("marker survived a failed apply")
	}
	if err := Check(context.Background(), te.Env); err == nil {
		t.Error("Check passed after a successful apply followed by a failed one")
	}
}

func TestBackendTable(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		setup func(t *testing.T, te *testEnv)
		// applyErr and checkErr are substrings; "" means success.
		applyErr string
		checkErr string
	}{
		"networkd record": {
			setup: func(t *testing.T, te *testEnv) { te.write(t, te.Paths.Backend, "networkd\n") },
		},
		"networkd record ignores a broken state file": {
			setup: func(t *testing.T, te *testEnv) {
				te.write(t, te.Paths.Backend, "networkd\n")
				te.write(t, te.Paths.State, "{")
			},
		},
		"no record, legacy networkd files": {
			setup: func(t *testing.T, te *testEnv) {
				te.write(t, filepath.Join(te.Paths.NetworkdDir, "10-spinifex-mgmt.network"), "")
				te.remote = "tcp:127.0.0.1:6642"
			},
		},
		"no record, nothing set up": {},
		"no record, set up without networkd files": {
			setup:    func(_ *testing.T, te *testEnv) { te.remote = "tcp:127.0.0.1:6642" },
			applyErr: "has no backend record",
			checkErr: "has no backend record",
		},
		"no record, OVS unreachable": {
			setup:    func(_ *testing.T, te *testEnv) { te.remoteErr = errors.New("database connection failed") },
			applyErr: "cannot tell whether this node is set up",
			checkErr: "cannot tell whether this node is set up",
		},
		"unknown backend": {
			setup:    func(t *testing.T, te *testEnv) { te.write(t, te.Paths.Backend, "netplan\n") },
			applyErr: `unknown backend "netplan"`,
			checkErr: `unknown backend "netplan"`,
		},
		"hostnet, state missing": {
			setup:    func(t *testing.T, te *testEnv) { te.write(t, te.Paths.Backend, "hostnet\n") },
			applyErr: "no such file",
			checkErr: "no such file",
		},
		"hostnet, state unreadable": {
			setup: func(t *testing.T, te *testEnv) {
				te.write(t, te.Paths.Backend, "hostnet\n")
				if err := os.Mkdir(te.Paths.State, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			applyErr: "is a directory",
			checkErr: "is a directory",
		},
		"hostnet, state truncated": {
			setup:    func(t *testing.T, te *testEnv) { te.hostnetBackend(t, vethState[:40]) },
			applyErr: "not valid JSON",
			checkErr: "not valid JSON",
		},
		"hostnet, unknown version": {
			setup: func(t *testing.T, te *testEnv) {
				te.hostnetBackend(t, strings.Replace(natState, `"version": 1`, `"version": 2`, 1))
			},
			applyErr: "unknown version 2",
			checkErr: "unknown version 2",
		},
		"hostnet, pending": {
			setup: func(t *testing.T, te *testEnv) {
				te.hostnetBackend(t, strings.Replace(natState, `"pending": false`, `"pending": true`, 1))
			},
			applyErr: "pending",
			checkErr: "pending",
		},
		"hostnet, never applied": {
			setup:    func(t *testing.T, te *testEnv) { te.hostnetBackend(t, natState) },
			checkErr: "host network not applied",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost().
				add(Link{Name: "br-int", Kind: "openvswitch"}).
				add(Link{Name: "br-ext", Kind: "openvswitch"})
			// Check runs before Apply so "never applied" sees no marker.
			te := newTestEnv(t, h)
			if tc.setup != nil {
				tc.setup(t, te)
			}
			wantErr(t, "Check", Check(ctx, te.Env), tc.checkErr)
			wantErr(t, "Apply", Apply(ctx, te.Env), tc.applyErr)
			if tc.applyErr != "" && te.markerExists() {
				t.Error("marker written after a failed apply")
			}
		})
	}
}

func wantErr(t *testing.T, what string, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Errorf("%s: unexpected error %v", what, err)
	case want != "" && err == nil:
		t.Errorf("%s: succeeded, want an error containing %q", what, want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Errorf("%s: error %q does not contain %q", what, err, want)
	}
}

func TestLoadStateRejectsInvalidFields(t *testing.T) {
	for name, tc := range map[string]struct {
		from, to, want string
	}{
		"unknown mode":       {`"mode": "nat"`, `"mode": "bonded"`, `wan.mode: unknown mode "bonded"`},
		"long name":          {`"ovs_bridge": "br-ext"`, `"ovs_bridge": "br-ext-much-too-long"`, "wan.ovs_bridge"},
		"slash in name":      {`"host_end": "spx-nat-host"`, `"host_end": "spx/nat"`, "wan.host_end"},
		"bad MAC":            {`"host_mac": "02:00:64:7f:00:01"`, `"host_mac": "02:00"`, "wan.host_mac"},
		"bad CIDR":           {`"host_cidr": "100.127.0.1/24"`, `"host_cidr": "100.127.0.1"`, "wan.host_cidr"},
		"unknown field":      {`"pending": false`, `"pending": false, "extra": 1`, `unknown field "extra"`},
		"missing int bridge": {`"int_bridge": "br-int"`, `"int_bridge": ""`, "int_bridge: missing"},
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(natState, tc.from) {
				t.Fatalf("fixture lacks %q", tc.from)
			}
			path := filepath.Join(t.TempDir(), "hostnet.json")
			if err := os.WriteFile(path, []byte(strings.Replace(natState, tc.from, tc.to, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadState(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
				t.Fatalf("LoadState error = %v, want it to name %s and contain %q", err, path, tc.want)
			}
		})
	}
}
