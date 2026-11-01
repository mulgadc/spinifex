package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	telemetryv1 "github.com/mulgadc/spinifex/contracts/telemetry/v1"
	"github.com/mulgadc/spinifex/spinifex/runtime/host/nettest"
	"github.com/mulgadc/spinifex/spinifex/runtime/host/nodefacts"
	"github.com/nats-io/nats.go"
)

// Build-time version and commit, set by the cmd package before the daemon starts.
var (
	buildVersion = "dev"
	buildCommit  = "unknown"
)

// SetBuildInfo records the build-time version and commit the node reports.
func SetBuildInfo(version, commit string) {
	buildVersion, buildCommit = version, commit
}

// nodeFactsTTL bounds how often a request rebuilds the facts; a re-cabled
// port shows within it.
const nodeFactsTTL = 15 * time.Second

// nodeInsight serves the operator's view of this host: its facts, a live
// sampler of its own load, and the two halves of a throughput test.
type nodeInsight struct {
	node string
	env  nodefacts.Env

	mu      sync.Mutex
	facts   clusterv1.NodeFacts
	factsAt time.Time

	live   *nodeLiveSampler
	server nettest.Server
	// testing serialises the client side, so two tests never share a link.
	testing sync.Mutex
}

func newNodeInsight(node string) *nodeInsight {
	return &nodeInsight{node: node, env: nodefacts.DefaultEnv(node, buildVersion, buildCommit)}
}

func (n *nodeInsight) currentFacts(ctx context.Context) clusterv1.NodeFacts {
	n.mu.Lock()
	defer n.mu.Unlock()
	if time.Since(n.factsAt) < nodeFactsTTL {
		return n.facts
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n.facts = nodefacts.Build(ctx, n.env)
	n.factsAt = time.Now()
	return n.facts
}

func (n *nodeInsight) planeAddress(ctx context.Context, plane string) string {
	addr, _ := n.planeSubnet(ctx, plane)
	return addr
}

// planeSubnet is the node's address on plane and the subnet it sits in. A
// client may only be pointed inside that subnet, never at an arbitrary host.
func (n *nodeInsight) planeSubnet(ctx context.Context, plane string) (string, *net.IPNet) {
	for _, p := range n.currentFacts(ctx).Planes {
		if p.Plane != plane || p.Address == "" {
			continue
		}
		_, subnet, err := net.ParseCIDR(p.Address + "/" + strconv.Itoa(p.Prefix))
		if err != nil {
			return p.Address, nil
		}
		return p.Address, subnet
	}
	return "", nil
}

func (n *nodeInsight) planeLink(ctx context.Context, plane string) (speed, mtu int) {
	f := n.currentFacts(ctx)
	for _, p := range f.Planes {
		if p.Plane != plane {
			continue
		}
		mtu = p.MTU
		for _, nic := range f.NICs {
			if slices.Contains(p.NICs, nic.Name) {
				speed = max(speed, nic.SpeedMbps)
			}
		}
	}
	return speed, mtu
}

func (d *Daemon) handleNodeFacts(msg *nats.Msg) string {
	respondWithJSON(d.node, msg, d.insight.currentFacts(context.Background()))
	return outcomeSuccess
}

func validPlane(plane string) bool {
	return plane == clusterv1.PlaneWAN || plane == clusterv1.PlaneLAN || plane == clusterv1.PlaneVPC
}

func (d *Daemon) handleNetTestServe(msg *nats.Msg) string {
	var req clusterv1.NetTestServeRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil || !validPlane(req.Plane) ||
		req.DurationSeconds < nettest.MinDuration || req.DurationSeconds > nettest.MaxDuration {
		respondWithJSON(d.node, msg, clusterv1.NetTestServeResponse{Error: "invalid request"})
		return outcomeError
	}
	addr := d.insight.planeAddress(context.Background(), req.Plane)
	if addr == "" {
		respondWithJSON(d.node, msg, clusterv1.NetTestServeResponse{Error: "this node has no address on the " + req.Plane + " plane"})
		return outcomeError
	}
	if err := d.insight.server.Serve(addr, req.DurationSeconds); err != nil {
		respondWithJSON(d.node, msg, clusterv1.NetTestServeResponse{Error: err.Error()})
		return outcomeError
	}
	respondWithJSON(d.node, msg, clusterv1.NetTestServeResponse{Address: addr, Port: clusterv1.NetTestPort})
	return outcomeSuccess
}

// handleNetTestRun runs the client in its own goroutine: it takes the test's
// duration, and the subscription must keep serving other requests meanwhile.
func (d *Daemon) handleNetTestRun(msg *nats.Msg) string {
	var req clusterv1.NetTestRunRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil || !validPlane(req.Plane) ||
		req.DurationSeconds < nettest.MinDuration || req.DurationSeconds > nettest.MaxDuration ||
		req.Streams < 1 || req.Streams > nettest.MaxStreams || req.Port != clusterv1.NetTestPort {
		respondWithJSON(d.node, msg, clusterv1.NetTestRunResponse{Error: "invalid request"})
		return outcomeError
	}
	go func() {
		ctx := context.Background()
		addr, subnet := d.insight.planeSubnet(ctx, req.Plane)
		if addr == "" {
			respondWithJSON(d.node, msg, clusterv1.NetTestRunResponse{Error: "this node has no address on the " + req.Plane + " plane"})
			return
		}
		if target := net.ParseIP(req.Address); subnet == nil || target == nil || !subnet.Contains(target) {
			respondWithJSON(d.node, msg, clusterv1.NetTestRunResponse{Error: "target is outside this node's " + req.Plane + " subnet"})
			return
		}
		if !d.insight.testing.TryLock() {
			respondWithJSON(d.node, msg, clusterv1.NetTestRunResponse{Error: "a test is already running from this node"})
			return
		}
		defer d.insight.testing.Unlock()

		res, err := nettest.Run(ctx, req, addr)
		res.LinkMbps, res.MTU = d.insight.planeLink(ctx, req.Plane)
		if err != nil {
			res.Error = err.Error()
			slog.Info("node network test failed", "plane", req.Plane, "target", req.Address, "err", err)
		}
		respondWithJSON(d.node, msg, res)
	}()
	return outcomeSuccess
}

// nodeLiveSampler samples this host every period while somebody watches, as
// the instance sampler does, and drops its ring when the lease lapses.
type nodeLiveSampler struct {
	insight *nodeInsight
	nc      *nats.Conn
	subject string

	mu      sync.Mutex
	until   time.Time
	running bool
	ring    []telemetryv1.NodeLiveSample
}

func (d *Daemon) handleNodeLiveWatch(msg *nats.Msg) string {
	var watch telemetryv1.LiveWatch
	if len(msg.Data) > 0 {
		if err := json.Unmarshal(msg.Data, &watch); err != nil {
			return outcomeError
		}
	}
	samples := d.insight.live.watch(watch.SinceMillis)
	if msg.Reply != "" {
		data, err := json.Marshal(telemetryv1.NodeLiveRing{Samples: samples})
		if err == nil {
			_ = msg.Respond(data)
		}
	}
	return outcomeSuccess
}

func (l *nodeLiveSampler) watch(sinceMillis int64) []telemetryv1.NodeLiveSample {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.until = time.Now().Add(telemetryv1.LiveWatchLeaseMillis * time.Millisecond)
	if !l.running {
		l.running = true
		go l.run()
	}
	out := make([]telemetryv1.NodeLiveSample, 0, len(l.ring))
	for _, s := range l.ring {
		if s.TimeMillis > sinceMillis {
			out = append(out, s)
		}
	}
	return out
}

func (l *nodeLiveSampler) lapsed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Now().Before(l.until) {
		return false
	}
	l.running = false
	l.ring = nil
	return true
}

func (l *nodeLiveSampler) record(s telemetryv1.NodeLiveSample) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ring) == telemetryv1.LiveRingSamples {
		l.ring = append(l.ring[:0], l.ring[1:]...)
	}
	l.ring = append(l.ring, s)
}

func (l *nodeLiveSampler) run() {
	ticker := time.NewTicker(telemetryv1.LiveSamplePeriodMillis * time.Millisecond)
	defer ticker.Stop()
	var prev *hostCounters
	for range ticker.C {
		if l.lapsed() {
			return
		}
		cur := readHostCounters(l.insight.env.ProcRoot, l.insight.env.SysRoot, planeNICs(l.insight.currentFacts(context.Background())))
		if prev != nil {
			if s, ok := hostRates(prev, cur); ok {
				l.record(s)
				if data, err := json.Marshal(s); err == nil {
					_ = l.nc.Publish(l.subject, data)
				}
			}
		}
		prev = cur
	}
}

// planeNICs keys each set of NICs by the planes it carries, so a NIC that
// carries lan and vpc is counted once, as "lan+vpc".
func planeNICs(f clusterv1.NodeFacts) map[string][]string {
	byNICs := map[string][]string{}
	nics := map[string][]string{}
	for _, p := range f.Planes {
		key := strings.Join(p.NICs, ",")
		if key == "" {
			key = "bridge:" + p.Bridge
		}
		byNICs[key] = append(byNICs[key], p.Plane)
		if _, ok := nics[key]; !ok {
			if len(p.NICs) == 0 {
				nics[key] = []string{p.Bridge}
			} else {
				nics[key] = p.NICs
			}
		}
	}
	out := map[string][]string{}
	for key, planes := range byNICs {
		out[strings.Join(planes, "+")] = nics[key]
	}
	return out
}

type nicCounters struct{ rxBytes, txBytes, rxPackets, txPackets uint64 }

type hostCounters struct {
	at         time.Time
	busy, idle uint64
	memPercent float64
	load1      float64
	planes     map[string]nicCounters
}

func readHostCounters(procRoot, sysRoot string, planes map[string][]string) *hostCounters {
	c := &hostCounters{at: time.Now(), planes: map[string]nicCounters{}}
	if data, err := os.ReadFile(filepath.Join(procRoot, "stat")); err == nil {
		line, _, _ := strings.Cut(string(data), "\n")
		fields := strings.Fields(line)
		for i, f := range fields[1:] {
			v, _ := strconv.ParseUint(f, 10, 64)
			// user nice system idle iowait irq softirq steal; guest time is
			// already counted in user.
			switch {
			case i == 3 || i == 4:
				c.idle += v
			case i < 8:
				c.busy += v
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(procRoot, "meminfo")); err == nil {
		var total, avail float64
		for line := range strings.SplitSeq(string(data), "\n") {
			key, value, _ := strings.Cut(line, ":")
			fields := strings.Fields(value)
			if len(fields) == 0 {
				continue
			}
			v, _ := strconv.ParseFloat(fields[0], 64)
			switch key {
			case "MemTotal":
				total = v
			case "MemAvailable":
				avail = v
			}
		}
		if total > 0 {
			c.memPercent = (total - avail) / total * 100
		}
	}
	if data, err := os.ReadFile(filepath.Join(procRoot, "loadavg")); err == nil {
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			c.load1, _ = strconv.ParseFloat(fields[0], 64)
		}
	}
	for plane, nics := range planes {
		var sum nicCounters
		for _, nic := range nics {
			read := func(name string) uint64 {
				data, err := os.ReadFile(filepath.Join(sysRoot, "class", "net", nic, "statistics", name))
				if err != nil {
					return 0
				}
				v, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
				return v
			}
			sum.rxBytes += read("rx_bytes")
			sum.txBytes += read("tx_bytes")
			sum.rxPackets += read("rx_packets")
			sum.txPackets += read("tx_packets")
		}
		c.planes[plane] = sum
	}
	return c
}

func hostRates(prev, cur *hostCounters) (telemetryv1.NodeLiveSample, bool) {
	elapsed := cur.at.Sub(prev.at).Seconds()
	if elapsed <= 0 || cur.busy < prev.busy || cur.idle < prev.idle {
		return telemetryv1.NodeLiveSample{}, false
	}
	s := telemetryv1.NodeLiveSample{
		TimeMillis:        cur.at.UnixMilli(),
		MemoryUsedPercent: round2(cur.memPercent),
		Load1:             cur.load1,
		Planes:            map[string]telemetryv1.NodeLivePlane{},
	}
	if total := float64((cur.busy - prev.busy) + (cur.idle - prev.idle)); total > 0 {
		s.CPUPercent = round2(float64(cur.busy-prev.busy) / total * 100)
	}
	rate := func(a, b uint64) float64 {
		if b < a {
			return 0
		}
		return round2(float64(b-a) / elapsed)
	}
	for plane, c := range cur.planes {
		p, ok := prev.planes[plane]
		if !ok {
			continue
		}
		s.Planes[plane] = telemetryv1.NodeLivePlane{
			NetworkIn: rate(p.rxBytes, c.rxBytes), NetworkOut: rate(p.txBytes, c.txBytes),
			PacketsIn: rate(p.rxPackets, c.rxPackets), PacketsOut: rate(p.txPackets, c.txPackets),
		}
	}
	return s, true
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
