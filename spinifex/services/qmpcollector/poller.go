package qmpcollector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
)

// userHZ is the kernel jiffy rate exposed in /proc/<pid>/stat. USER_HZ is 100
// on every Linux architecture spinifex targets (x86_64, aarch64).
const userHZ = 100.0

// sample is one raw counter snapshot; published values are deltas between
// consecutive samples.
type sample struct {
	at         time.Time
	cpuJiffies uint64 // Σ utime+stime across vCPU threads
	rdBytes    int64  // cumulative, summed across block devices
	wrBytes    int64
	rdOps      int64
	wrOps      int64
	rxBytes    uint64 // tap rx = guest egress (NetworkOut)
	txBytes    uint64 // tap tx = guest ingress (NetworkIn)
	balloonOK  bool
	balloon    int64 // current guest memory bytes (gauge)
}

// poller collects one VM on its monitoring period, staggered per instance.
type poller struct {
	cfg    *Config
	nc     *nats.Conn
	cancel context.CancelFunc

	mu   sync.Mutex
	meta types.GuestTelemetryMeta
	prev *sample
}

func newPoller(cfg *Config, nc *nats.Conn, meta types.GuestTelemetryMeta) *poller {
	return &poller{cfg: cfg, nc: nc, meta: meta}
}

// updateMeta refreshes taps/period after an ENI hot-plug rewrite.
func (p *poller) updateMeta(meta types.GuestTelemetryMeta) {
	p.mu.Lock()
	p.meta = meta
	p.mu.Unlock()
}

func (p *poller) snapshotMeta() types.GuestTelemetryMeta {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.meta
}

func (p *poller) period() time.Duration {
	secs := p.snapshotMeta().PeriodSeconds
	if secs != 60 {
		secs = 300 // basic tier; also the safe default for absent/garbage values
	}
	return time.Duration(secs) * time.Second
}

func (p *poller) start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)
	go p.run(ctx)
}

func (p *poller) stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

func (p *poller) run(ctx context.Context) {
	period := p.period()
	select {
	case <-ctx.Done():
		return
	case <-time.After(staggerOffset(p.meta.InstanceID, period)):
	}

	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		p.tick()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if next := p.period(); next != period {
			period = next
			ticker.Reset(period)
		}
	}
}

// tick takes a snapshot and publishes the delta against the previous one.
// The first snapshot only primes counters; any error or counter regression
// (QEMU restarted) re-primes instead of publishing garbage.
func (p *poller) tick() {
	meta := p.snapshotMeta()
	cur, err := p.collect(meta)
	if err != nil {
		slog.Debug("qmp-collector: collect failed, re-priming",
			"instanceId", meta.InstanceID, "err", err)
		p.mu.Lock()
		p.prev = nil
		p.mu.Unlock()
		return
	}

	p.mu.Lock()
	prev := p.prev
	p.prev = cur
	p.mu.Unlock()
	if prev == nil {
		return
	}

	batch, ok := buildBatch(meta, p.cfg.NodeName, prev, cur)
	if !ok {
		return
	}
	data, err := json.Marshal(batch)
	if err != nil {
		slog.Error("qmp-collector: marshal batch", "instanceId", meta.InstanceID, "err", err)
		return
	}
	if err := p.nc.Publish(types.MetricsEC2SubjectPrefix+meta.InstanceID, data); err != nil {
		slog.Warn("qmp-collector: publish failed", "instanceId", meta.InstanceID, "err", err)
	}
}

// collect dials the telemetry socket fresh each tick (no held connection to
// leak across QEMU restarts) and reads QMP + procfs + sysfs counters.
func (p *poller) collect(meta types.GuestTelemetryMeta) (*sample, error) {
	client, err := qmp.NewQMPClient(meta.Socket)
	if err != nil {
		return nil, fmt.Errorf("dial telemetry socket: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Execute(qmp.QMPCommand{Execute: "qmp_capabilities"}, nil); err != nil {
		return nil, fmt.Errorf("qmp_capabilities: %w", err)
	}

	var cpus []qmp.CPUInfoFast
	if err := client.Execute(qmp.QMPCommand{Execute: "query-cpus-fast"}, &cpus); err != nil {
		return nil, fmt.Errorf("query-cpus-fast: %w", err)
	}
	var stats []qmp.BlockStats
	if err := client.Execute(qmp.QMPCommand{Execute: "query-blockstats"}, &stats); err != nil {
		return nil, fmt.Errorf("query-blockstats: %w", err)
	}

	s := &sample{at: time.Now()}

	// Balloon is optional hardware; absence just drops the memory series.
	var balloon qmp.BalloonInfo
	if err := client.Execute(qmp.QMPCommand{Execute: "query-balloon"}, &balloon); err == nil {
		s.balloonOK = true
		s.balloon = balloon.Actual
	}

	for _, cpu := range cpus {
		jiffies, err := threadJiffies(p.cfg.ProcRoot, cpu.ThreadID)
		if err != nil {
			return nil, fmt.Errorf("vCPU %d thread %d: %w", cpu.CPUIndex, cpu.ThreadID, err)
		}
		s.cpuJiffies += jiffies
	}

	for _, bs := range stats {
		s.rdBytes += bs.Stats.RdBytes
		s.wrBytes += bs.Stats.WrBytes
		s.rdOps += bs.Stats.RdOperations
		s.wrOps += bs.Stats.WrOperations
	}

	// A tap that disappeared mid-interval (hot-unplug race) is skipped; the
	// next metadata refresh drops it for good.
	for _, tap := range meta.Taps {
		rx, err := readSysfsCounter(p.cfg.SysRoot, tap, "rx_bytes")
		if err != nil {
			continue
		}
		tx, err := readSysfsCounter(p.cfg.SysRoot, tap, "tx_bytes")
		if err != nil {
			continue
		}
		s.rxBytes += rx
		s.txBytes += tx
	}

	return s, nil
}

// threadJiffies returns utime+stime for a host thread from /proc/<tid>/stat.
// Fields are parsed after the last ')' so a comm containing spaces or parens
// cannot shift positions: utime and stime are then fields 12 and 13 (0-based)
// of the remainder.
func threadJiffies(procRoot string, tid int) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(tid), "stat"))
	if err != nil {
		return 0, err
	}
	raw := string(data)
	idx := strings.LastIndexByte(raw, ')')
	if idx < 0 {
		return 0, fmt.Errorf("malformed stat for tid %d", tid)
	}
	fields := strings.Fields(raw[idx+1:])
	if len(fields) < 14 {
		return 0, fmt.Errorf("short stat for tid %d: %d fields", tid, len(fields))
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse utime for tid %d: %w", tid, err)
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse stime for tid %d: %w", tid, err)
	}
	return utime + stime, nil
}

func readSysfsCounter(sysRoot, iface, counter string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(sysRoot, "class", "net", iface, "statistics", counter))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

// buildBatch converts two snapshots into the locked goanna_ec2_* series set.
// ok is false when a counter regressed (QEMU restart between ticks) — the
// fresh snapshot then serves as the new baseline and nothing is published.
func buildBatch(meta types.GuestTelemetryMeta, node string, prev, cur *sample) (types.TelemetryBatch, bool) {
	elapsed := cur.at.Sub(prev.at).Seconds()
	if elapsed <= 0 {
		return types.TelemetryBatch{}, false
	}
	if cur.cpuJiffies < prev.cpuJiffies ||
		cur.rdBytes < prev.rdBytes || cur.wrBytes < prev.wrBytes ||
		cur.rdOps < prev.rdOps || cur.wrOps < prev.wrOps ||
		cur.rxBytes < prev.rxBytes || cur.txBytes < prev.txBytes {
		return types.TelemetryBatch{}, false
	}

	labels := map[string]string{
		"namespace":   "AWS/EC2",
		"instance_id": meta.InstanceID,
	}
	if meta.AccountID != "" {
		labels["account_id"] = meta.AccountID
	}

	vcpus := max(meta.VCPUs, 1)
	cpuPct := float64(cur.cpuJiffies-prev.cpuJiffies) / userHZ / elapsed / float64(vcpus) * 100
	if cpuPct > 100 {
		cpuPct = 100
	}

	series := []types.TelemetrySeries{
		{Name: "goanna_ec2_cpu_utilization", Labels: labels, Value: cpuPct, Unit: "Percent"},
		{Name: "goanna_ec2_network_in_bytes", Labels: labels, Value: float64(cur.txBytes - prev.txBytes), Unit: "Bytes"},
		{Name: "goanna_ec2_network_out_bytes", Labels: labels, Value: float64(cur.rxBytes - prev.rxBytes), Unit: "Bytes"},
		{Name: "goanna_ec2_disk_read_bytes", Labels: labels, Value: float64(cur.rdBytes - prev.rdBytes), Unit: "Bytes"},
		{Name: "goanna_ec2_disk_write_bytes", Labels: labels, Value: float64(cur.wrBytes - prev.wrBytes), Unit: "Bytes"},
		{Name: "goanna_ec2_disk_read_ops", Labels: labels, Value: float64(cur.rdOps - prev.rdOps), Unit: "Count"},
		{Name: "goanna_ec2_disk_write_ops", Labels: labels, Value: float64(cur.wrOps - prev.wrOps), Unit: "Count"},
	}
	if cur.balloonOK {
		series = append(series, types.TelemetrySeries{
			Name: "goanna_ec2_memory_actual_bytes", Labels: labels,
			Value: float64(cur.balloon), Unit: "Bytes",
		})
	}

	return types.TelemetryBatch{
		TS:            cur.at.Unix(),
		PeriodSeconds: int(elapsed + 0.5),
		Node:          node,
		Series:        series,
	}, true
}
