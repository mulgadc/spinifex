package hostnet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// scriptRunner answers ip commands from canned output keyed by the joined
// command line, and records every command it is asked to run.
type scriptRunner struct {
	out  map[string]string
	runs []string
}

func (r *scriptRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := name + " " + strings.Join(args, " ")
	r.runs = append(r.runs, line)
	if out, ok := r.out[line]; ok {
		return []byte(out), nil
	}
	if strings.HasPrefix(line, "ip -j") {
		return nil, fmt.Errorf("unexpected query %q", line)
	}
	return nil, nil
}

// Captured from iproute2 on Debian 13.
const ipLinkJSON = `[
 {"ifindex":13,"link":"veth-wan-ovs","ifname":"veth-wan-br","flags":["BROADCAST","MULTICAST","UP","LOWER_UP"],"master":"br-wan","address":"1E:90:24:9F:95:0C","linkinfo":{"info_kind":"veth","info_slave_kind":"bridge"}},
 {"ifindex":12,"link":"veth-wan-br","ifname":"veth-wan-ovs","flags":["BROADCAST","MULTICAST","UP","LOWER_UP"],"master":"ovs-system","address":"5a:1c:00:00:00:01","linkinfo":{"info_kind":"veth","info_slave_kind":"openvswitch"}},
 {"ifindex":9,"ifname":"br-int","flags":["BROADCAST","MULTICAST"],"address":"6e:00:00:00:00:02","linkinfo":{"info_kind":"openvswitch"}},
 {"ifindex":2,"ifname":"eth1","flags":["BROADCAST","MULTICAST","UP"],"address":"52:54:00:00:00:03"}
]`

func TestIPHostLink(t *testing.T) {
	r := &scriptRunner{out: map[string]string{"ip -j -d link show": ipLinkJSON}}
	h := &IPHost{Runner: r}
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		want *Link
	}{
		{"veth-wan-br", &Link{Name: "veth-wan-br", Kind: "veth", Peer: "veth-wan-ovs", Master: "br-wan", MAC: "1e:90:24:9f:95:0c", Up: true}},
		{"br-int", &Link{Name: "br-int", Kind: "openvswitch", MAC: "6e:00:00:00:00:02"}},
		{"eth1", &Link{Name: "eth1", MAC: "52:54:00:00:00:03", Up: true}},
		{"br-wan-missing", nil},
	} {
		got, err := h.Link(ctx, tc.name)
		if err != nil {
			t.Fatalf("Link(%s): %v", tc.name, err)
		}
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("Link(%s) = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestIPHostAddrsAndRoutes(t *testing.T) {
	r := &scriptRunner{out: map[string]string{
		"ip -j -4 addr show dev br-mgmt":         `[{"ifname":"br-mgmt","addr_info":[{"family":"inet","local":"10.15.8.1","prefixlen":24}]}]`,
		"ip -j -4 route show exact 10.15.8.0/24": `[{"dst":"10.15.8.0/24","dev":"br-mgmt","protocol":"kernel","scope":"link","prefsrc":"10.15.8.1","flags":[]},{"dst":"10.15.8.0/24","dev":"br-mgmt","prefsrc":"10.15.8.1","metric":50,"flags":[]}]`,
	}}
	h := &IPHost{Runner: r}
	ctx := context.Background()

	addrs, err := h.IPv4Addrs(ctx, "br-mgmt")
	if err != nil || !slices.Equal(addrs, []string{"10.15.8.1/24"}) {
		t.Errorf("IPv4Addrs = %q, %v", addrs, err)
	}
	routes, err := h.Routes(ctx, "10.15.8.0/24")
	want := []RouteEntry{{Dev: "br-mgmt", Src: "10.15.8.1"}, {Dev: "br-mgmt", Src: "10.15.8.1", Metric: 50}}
	if err != nil || !slices.Equal(routes, want) {
		t.Errorf("Routes = %+v, %v; want %+v", routes, err, want)
	}
}

// TestApplyNeverDeletesALink runs a full apply through IPHost, so the check is
// on the ip commands themselves, not on the Host interface.
func TestApplyNeverDeletesALink(t *testing.T) {
	r := &scriptRunner{out: map[string]string{
		"ip -j -d link show": `[
 {"ifname":"br-int","flags":[],"linkinfo":{"info_kind":"openvswitch"}},
 {"ifname":"br-ext","flags":[],"linkinfo":{"info_kind":"openvswitch"}},
 {"ifname":"br-wan","flags":["UP"],"linkinfo":{"info_kind":"bridge"}},
 {"ifname":"br-mgmt","flags":[],"linkinfo":{"info_kind":"openvswitch"}},
 {"ifname":"br-vpc","flags":["UP"],"linkinfo":{"info_kind":"bridge"}},
 {"ifname":"mgmt","flags":[]}
]`,
		"ip -j -4 addr show dev br-mgmt":        `[]`,
		"ip -j -4 addr show dev mgmt":           `[{"addr_info":[{"local":"169.254.3.4","prefixlen":16}]}]`,
		"ip -j -4 addr show dev br-vpc":         `[{"addr_info":[{"local":"10.1.0.5","prefixlen":16}]}]`,
		"ip -j -4 route show exact 10.1.0.0/16": `[]`,
	}}
	te := newTestEnv(t, nil)
	te.Host = &IPHost{Runner: r}
	te.hostnetBackend(t, vethState)

	// The canned link list never gains the veth pair, so this apply fails
	// once it looks for it again; every command up to then is still checked.
	_ = Apply(context.Background(), te.Env)
	if !slices.Contains(r.runs, "ip link add veth-wan-br type veth peer name veth-wan-ovs") {
		t.Fatalf("apply did not reach the veth pair: %q", r.runs)
	}
	for _, run := range r.runs {
		f := strings.Fields(run)
		if len(f) >= 3 && f[1] == "link" && (f[2] == "del" || f[2] == "delete") {
			t.Errorf("apply deleted a link: %q", run)
		}
	}
}

func TestApplyTakesLockBeforeReadingState(t *testing.T) {
	te := newTestEnv(t, newFakeHost())
	te.LockWait = 2 * time.Second
	te.hostnetBackend(t, "{")

	held := holdLock(t, te.Paths.Lock)
	// Replace the broken state while Apply waits on the lock: it must read the
	// replacement, so the wait is where the state file changes.
	te.Sleep = func(time.Duration) {
		te.write(t, te.Paths.State, natState)
		te.host.add(Link{Name: "br-int", Kind: "openvswitch"}).add(Link{Name: "br-ext", Kind: "openvswitch"})
		held.Close()
	}
	if err := Apply(context.Background(), te.Env); err != nil {
		t.Fatalf("Apply read the state before taking the lock: %v", err)
	}
}

func TestApplyLockTimeoutNamesHolder(t *testing.T) {
	te := newTestEnv(t, newFakeHost())
	te.hostnetBackend(t, natState)
	held := holdLock(t, te.Paths.Lock)
	defer held.Close()

	err := Apply(context.Background(), te.Env)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("still held after 2m0s by PID %d", os.Getpid())) {
		t.Fatalf("Apply error = %v, want it to name this test's PID", err)
	}
	if te.slept < DefaultLockWait {
		t.Errorf("gave up after %s, before the %s wait", te.slept, DefaultLockWait)
	}
}

// holdLock takes the hostnet flock on a separate open file description, as
// setup-ovn.sh does from another process.
func holdLock(t *testing.T, path string) *os.File {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return f
}
