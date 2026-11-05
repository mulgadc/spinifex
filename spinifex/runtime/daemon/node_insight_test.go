//test:in-package — the handlers, the plane lookups and the live sampler are
//unexported, and the fixture seeds the cached facts directly so no test reads
//this host's hardware.

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	telemetryv1 "github.com/mulgadc/spinifex/contracts/telemetry/v1"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/runtime/host/nodefacts"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func insightFacts() clusterv1.NodeFacts {
	return clusterv1.NodeFacts{
		NICs: []clusterv1.NICFacts{
			{Name: "eth0", SpeedMbps: 25000},
			{Name: "eth1", SpeedMbps: 10000},
			{Name: "eth2", SpeedMbps: 1000},
		},
		Planes: []clusterv1.PlaneFacts{
			{Plane: clusterv1.PlaneLAN, Bridge: "br-lan", Address: "10.10.8.1", Prefix: 24, NICs: []string{"eth0", "eth1"}, MTU: 9000},
			{Plane: clusterv1.PlaneVPC, Bridge: "br-lan", Address: "10.10.8.1", Prefix: 24, NICs: []string{"eth0", "eth1"}, MTU: 9000},
			{Plane: clusterv1.PlaneWAN, Bridge: "br-wan", NICs: []string{"eth2"}},
		},
	}
}

func insightDaemon(t *testing.T, facts clusterv1.NodeFacts) (*Daemon, *nats.Conn) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	insight := newNodeInsight("node1", "", "")
	insight.facts, insight.factsAt = facts, time.Now()
	return &Daemon{node: "node1", natsConn: nc, insight: insight}, nc
}

func callHandler[T any](t *testing.T, nc *nats.Conn, handler func(*nats.Msg) string, req any) T {
	t.Helper()
	subject := nats.NewInbox()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) { handler(msg) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	data, err := json.Marshal(req)
	require.NoError(t, err)
	reply, err := nc.Request(subject, data, 5*time.Second)
	require.NoError(t, err)

	var out T
	require.NoError(t, json.Unmarshal(reply.Data, &out))
	return out
}

func validRun() clusterv1.NetTestRunRequest {
	return clusterv1.NetTestRunRequest{
		Plane: clusterv1.PlaneLAN, Target: "node2", Address: "10.10.8.2", Port: clusterv1.NetTestPort,
		DurationSeconds: 5, Streams: 4,
	}
}

// servePeerFacts answers for another node, as its daemon would.
func servePeerFacts(t *testing.T, nc *nats.Conn, node string, facts clusterv1.NodeFacts) {
	t.Helper()
	data, err := json.Marshal(facts)
	require.NoError(t, err)
	sub, err := nc.Subscribe(clusterv1.NodeFactsOfSubject(node), func(msg *nats.Msg) { _ = msg.Respond(data) })
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func peerFacts() clusterv1.NodeFacts {
	return clusterv1.NodeFacts{Node: "node2", Planes: []clusterv1.PlaneFacts{
		{Plane: clusterv1.PlaneLAN, Address: "10.10.8.2", Prefix: 24, NICs: []string{"eth0"}},
	}}
}

// A shared subnet is not a cluster: on a cloud the WAN subnet holds other
// tenants, so only the address the target node reports is admitted.
func TestHandleNetTestRun_RefusesAddressTheTargetDoesNotReport(t *testing.T) {
	t.Parallel()
	d, nc := insightDaemon(t, insightFacts())
	servePeerFacts(t, nc, "node2", peerFacts())

	for _, addr := range []string{"10.10.8.9", "10.10.9.2", "8.8.8.8", "127.0.0.1", ""} {
		req := validRun()
		req.Address = addr
		res := callHandler[clusterv1.NetTestRunResponse](t, nc, d.handleNetTestRun, req)
		assert.Equal(t, addr+" is not node2's lan address", res.Error, "address %q", addr)
	}
}

func TestHandleNetTestRun_RefusesATargetThatDoesNotAnswer(t *testing.T) {
	t.Parallel()
	d, nc := insightDaemon(t, insightFacts())

	res := callHandler[clusterv1.NetTestRunResponse](t, nc, d.handleNetTestRun, validRun())
	assert.Equal(t, "10.10.8.2 is not node2's lan address", res.Error)
}

func TestHandleNetTestRun_RefusesInvalidRequests(t *testing.T) {
	t.Parallel()
	d, nc := insightDaemon(t, insightFacts())

	mutations := map[string]func(*clusterv1.NetTestRunRequest){
		"unknown plane":  func(r *clusterv1.NetTestRunRequest) { r.Plane = "storage" },
		"short duration": func(r *clusterv1.NetTestRunRequest) { r.DurationSeconds = 1 },
		"long duration":  func(r *clusterv1.NetTestRunRequest) { r.DurationSeconds = 600 },
		"no streams":     func(r *clusterv1.NetTestRunRequest) { r.Streams = 0 },
		"many streams":   func(r *clusterv1.NetTestRunRequest) { r.Streams = 99 },
		"other port":     func(r *clusterv1.NetTestRunRequest) { r.Port = 22 },
		"no target":      func(r *clusterv1.NetTestRunRequest) { r.Target = "" },
		"self as target": func(r *clusterv1.NetTestRunRequest) { r.Target = "node1" },
		"subject target": func(r *clusterv1.NetTestRunRequest) { r.Target = "node2.>" },
	}
	for name, mutate := range mutations {
		req := validRun()
		mutate(&req)
		res := callHandler[clusterv1.NetTestRunResponse](t, nc, d.handleNetTestRun, req)
		assert.Equal(t, "invalid request", res.Error, name)
	}
}

func TestHandleNetTestRun_RefusesPlaneWithoutAddress(t *testing.T) {
	t.Parallel()
	d, nc := insightDaemon(t, insightFacts())

	req := validRun()
	req.Plane = clusterv1.PlaneWAN
	res := callHandler[clusterv1.NetTestRunResponse](t, nc, d.handleNetTestRun, req)
	assert.Equal(t, "this node has no address on the wan plane", res.Error)
}

func TestHandleNetTestRun_OneTestPerNode(t *testing.T) {
	t.Parallel()
	d, nc := insightDaemon(t, insightFacts())
	servePeerFacts(t, nc, "node2", peerFacts())
	d.insight.testing.Lock()
	defer d.insight.testing.Unlock()

	res := callHandler[clusterv1.NetTestRunResponse](t, nc, d.handleNetTestRun, validRun())
	assert.Equal(t, "a test is already running from this node", res.Error)
}

func TestHandleNetTestServe_RefusesBadRequests(t *testing.T) {
	t.Parallel()
	d, nc := insightDaemon(t, insightFacts())

	res := callHandler[clusterv1.NetTestServeResponse](t, nc, d.handleNetTestServe,
		clusterv1.NetTestServeRequest{Plane: clusterv1.PlaneLAN, DurationSeconds: 600})
	assert.Equal(t, "invalid request", res.Error)

	res = callHandler[clusterv1.NetTestServeResponse](t, nc, d.handleNetTestServe,
		clusterv1.NetTestServeRequest{Plane: clusterv1.PlaneWAN, DurationSeconds: 5})
	assert.Equal(t, "this node has no address on the wan plane", res.Error)
	assert.Zero(t, res.Port)
}

func TestHandleNodeFacts_ServesCachedFacts(t *testing.T) {
	t.Parallel()
	d, nc := insightDaemon(t, insightFacts())

	got := callHandler[clusterv1.NodeFacts](t, nc, d.handleNodeFacts, struct{}{})
	assert.Equal(t, insightFacts().Planes, got.Planes)
}

func TestPlaneLink_FastestNICAndMTU(t *testing.T) {
	t.Parallel()
	d, _ := insightDaemon(t, insightFacts())

	speed, mtu := d.insight.planeLink(t.Context(), clusterv1.PlaneLAN)
	assert.Equal(t, 25000, speed)
	assert.Equal(t, 9000, mtu)

	speed, mtu = d.insight.planeLink(t.Context(), "storage")
	assert.Zero(t, speed)
	assert.Zero(t, mtu)
}

func TestPlaneNICs_SharedNICsCountedOnce(t *testing.T) {
	t.Parallel()
	got := planeNICs(insightFacts())
	assert.Equal(t, map[string][]string{
		"lan+vpc": {"eth0", "eth1"},
		"wan":     {"eth2"},
	}, got)

	got = planeNICs(clusterv1.NodeFacts{Planes: []clusterv1.PlaneFacts{{Plane: clusterv1.PlaneWAN, Bridge: "br-wan"}}})
	assert.Equal(t, map[string][]string{"wan": {"br-wan"}}, got)
}

func writeHostTree(t *testing.T, root string, stat, rx string) {
	t.Helper()
	proc := filepath.Join(root, "proc")
	stats := filepath.Join(root, "sys", "class", "net", "eth0", "statistics")
	require.NoError(t, os.MkdirAll(proc, 0o755))
	require.NoError(t, os.MkdirAll(stats, 0o755))
	files := map[string]string{
		filepath.Join(proc, "stat"):        stat,
		filepath.Join(proc, "meminfo"):     "MemTotal: 1000 kB\nMemFree: 100 kB\nMemAvailable: 250 kB\n",
		filepath.Join(proc, "loadavg"):     "1.50 1.00 0.50 1/100 1\n",
		filepath.Join(stats, "rx_bytes"):   rx,
		filepath.Join(stats, "tx_bytes"):   "0\n",
		filepath.Join(stats, "rx_packets"): "0\n",
		filepath.Join(stats, "tx_packets"): "0\n",
	}
	for path, body := range files {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}
}

func TestHostRates_FromProcAndSysCounters(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	planes := map[string][]string{"lan": {"eth0", "missing0"}}

	writeHostTree(t, root, "cpu 100 0 100 700 100 0 0 0 0 0\ncpu0 1 1 1 1\n", "1000\n")
	prev := readHostCounters(filepath.Join(root, "proc"), filepath.Join(root, "sys"), planes)
	writeHostTree(t, root, "cpu 200 0 200 1300 200 0 0 0 0 0\n", "3000\n")
	cur := readHostCounters(filepath.Join(root, "proc"), filepath.Join(root, "sys"), planes)
	cur.at = prev.at.Add(2 * time.Second)

	s, ok := hostRates(prev, cur)
	require.True(t, ok)
	assert.InDelta(t, 22.22, s.CPUPercent, 0.01)
	assert.InDelta(t, 75.0, s.MemoryUsedPercent, 0.01)
	assert.InDelta(t, 1.5, s.Load1, 0.001)
	assert.InDelta(t, 1000.0, s.Planes["lan"].NetworkIn, 0.01)

	_, ok = hostRates(cur, prev)
	assert.False(t, ok, "counters going backwards are a reset, not a rate")
}

func TestNodeLiveSampler_RingAndLease(t *testing.T) {
	t.Parallel()
	l := &nodeLiveSampler{running: true}
	for i := range telemetryv1.LiveRingSamples + 5 {
		l.record(telemetryv1.NodeLiveSample{TimeMillis: int64(i)})
	}
	require.Len(t, l.ring, telemetryv1.LiveRingSamples)
	assert.Equal(t, int64(5), l.ring[0].TimeMillis)

	l.until = time.Now().Add(time.Minute)
	got := l.watch(int64(telemetryv1.LiveRingSamples + 2))
	assert.Len(t, got, 2)
	assert.False(t, l.lapsed())

	l.until = time.Now().Add(-time.Second)
	assert.True(t, l.lapsed())
	assert.Empty(t, l.ring)
	assert.False(t, l.running)
}

func TestHandleNodeLiveWatch_ReturnsRing(t *testing.T) {
	t.Parallel()
	d, nc := insightDaemon(t, insightFacts())
	d.insight.live = &nodeLiveSampler{insight: d.insight, nc: nc, subject: nats.NewInbox(), running: true}
	d.insight.live.record(telemetryv1.NodeLiveSample{TimeMillis: 10})
	d.insight.live.record(telemetryv1.NodeLiveSample{TimeMillis: 20})

	ring := callHandler[telemetryv1.NodeLiveRing](t, nc, d.handleNodeLiveWatch, telemetryv1.LiveWatch{SinceMillis: 10})
	require.Len(t, ring.Samples, 1)
	assert.Equal(t, int64(20), ring.Samples[0].TimeMillis)
}

// A hung mount blocks statfs where no context can reach it. The facts and the
// network-test lookups must still answer, with the mount reported on its row.
func TestHungMountLeavesFactsAndNetTestSetupAnswering(t *testing.T) {
	t.Parallel()
	d, nc := insightDaemon(t, clusterv1.NodeFacts{})
	root := t.TempDir()
	mountinfo := "22 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw\n" +
		"23 22 259:3 / /hung rw - ext4 /dev/nvme0n1p3 rw\n"
	require.NoError(t, os.MkdirAll(filepath.Join(root, "proc", "self"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "proc", "self", "mountinfo"), []byte(mountinfo), 0o644))
	d.insight.env = nodefacts.Env{Dir: root, SysRoot: filepath.Join(root, "sys"), ProcRoot: filepath.Join(root, "proc"), EtcRoot: filepath.Join(root, "etc")}
	d.insight.factsAt = time.Time{}

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	d.insight.usage.Deadline = 20 * time.Millisecond
	d.insight.usage.Statfs = func(path string, st *unix.Statfs_t) error {
		if path == "/hung" {
			<-release
		}
		st.Bsize, st.Frsize, st.Blocks, st.Bfree, st.Bavail = 4096, 4096, 100, 50, 50
		return nil
	}
	go d.insight.probeUsage(t.Context())

	require.Eventually(t, func() bool {
		d.insight.mu.Lock()
		d.insight.factsAt = time.Time{}
		d.insight.mu.Unlock()

		start := time.Now()
		got := callHandler[clusterv1.NodeFacts](t, nc, d.handleNodeFacts, struct{}{})
		d.insight.planeAddress(t.Context(), clusterv1.PlaneLAN)
		require.Less(t, time.Since(start), time.Second, "neither waits on the hung mount")

		require.Len(t, got.Filesystems, 2)
		return got.Filesystems[0].Usage != nil && got.Filesystems[0].Usage.SizeBytes == 100*4096 && got.Filesystems[1].Error == "not answering"
	}, 5*time.Second, 20*time.Millisecond)
}
