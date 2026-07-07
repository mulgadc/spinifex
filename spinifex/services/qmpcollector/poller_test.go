package qmpcollector

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/types"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestThreadJiffies(t *testing.T) {
	procRoot := t.TempDir()

	tests := []struct {
		name    string
		tid     int
		stat    string
		want    uint64
		wantErr bool
	}{
		{
			name: "plain comm",
			tid:  100,
			stat: "100 (qemu-system-x86) S 1 100 100 0 -1 4194560 500 0 0 0 1500 700 0 0 20 0 9 0 12345 1000000 200 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0",
			want: 2200,
		},
		{
			name: "comm with spaces and parens",
			tid:  101,
			stat: "101 (CPU 0/KVM) (x) R 1 101 101 0 -1 4194560 500 0 0 0 42 8 0 0 20 0 9 0 12345 1000000 200 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0",
			want: 50,
		},
		{
			name:    "missing tid",
			tid:     999,
			wantErr: true,
		},
		{
			name:    "short stat",
			tid:     102,
			stat:    "102 (x) S 1 2 3",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.stat != "" {
				writeFile(t, filepath.Join(procRoot, strconv.Itoa(tt.tid), "stat"), tt.stat)
			}
			got, err := threadJiffies(procRoot, tt.tid)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("jiffies = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestReadSysfsCounter(t *testing.T) {
	sysRoot := t.TempDir()
	writeFile(t, filepath.Join(sysRoot, "class/net/tap01/statistics/rx_bytes"), "123456\n")

	got, err := readSysfsCounter(sysRoot, "tap01", "rx_bytes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 123456 {
		t.Errorf("rx_bytes = %d, want 123456", got)
	}
	if _, err := readSysfsCounter(sysRoot, "tapgone", "rx_bytes"); err == nil {
		t.Error("expected error for missing tap")
	}
}

func TestBuildBatch(t *testing.T) {
	meta := types.GuestTelemetryMeta{
		InstanceID: "i-0abc", AccountID: "123456789012", VCPUs: 2, PeriodSeconds: 60,
	}
	base := time.Unix(1000, 0)
	prev := &sample{
		at: base, cpuJiffies: 1000,
		rdBytes: 100, wrBytes: 200, rdOps: 10, wrOps: 20,
		rxBytes: 5000, txBytes: 6000,
	}
	cur := &sample{
		// 60s elapsed, 2 vCPUs: 3000 jiffies of a 12000-jiffy budget = 25%.
		at: base.Add(60 * time.Second), cpuJiffies: 4000,
		rdBytes: 1100, wrBytes: 1200, rdOps: 60, wrOps: 45,
		rxBytes: 15000, txBytes: 26000,
		balloonOK: true, balloon: 1 << 30,
	}

	batch, ok := buildBatch(meta, "node1", prev, cur)
	if !ok {
		t.Fatal("expected batch")
	}
	if batch.Node != "node1" || batch.PeriodSeconds != 60 || batch.TS != cur.at.Unix() {
		t.Errorf("batch header = %+v", batch)
	}

	values := map[string]float64{}
	for _, s := range batch.Series {
		values[s.Name] = s.Value
		if s.Labels["namespace"] != "AWS/EC2" || s.Labels["instance_id"] != "i-0abc" ||
			s.Labels["account_id"] != "123456789012" {
			t.Errorf("%s labels = %v", s.Name, s.Labels)
		}
	}
	want := map[string]float64{
		"goanna_ec2_cpu_utilization":     25,
		"goanna_ec2_network_in_bytes":    20000, // tap tx = guest ingress
		"goanna_ec2_network_out_bytes":   10000, // tap rx = guest egress
		"goanna_ec2_disk_read_bytes":     1000,
		"goanna_ec2_disk_write_bytes":    1000,
		"goanna_ec2_disk_read_ops":       50,
		"goanna_ec2_disk_write_ops":      25,
		"goanna_ec2_memory_actual_bytes": 1 << 30,
	}
	for name, w := range want {
		if got, ok := values[name]; !ok || got != w {
			t.Errorf("%s = %v (present %v), want %v", name, got, ok, w)
		}
	}
	if len(batch.Series) != len(want) {
		t.Errorf("series count = %d, want %d", len(batch.Series), len(want))
	}
}

func TestBuildBatchCounterRegression(t *testing.T) {
	meta := types.GuestTelemetryMeta{InstanceID: "i-0abc", VCPUs: 1, PeriodSeconds: 60}
	base := time.Unix(1000, 0)
	prev := &sample{at: base, cpuJiffies: 5000, rdBytes: 900}
	// QEMU restarted: counters reset below prev.
	cur := &sample{at: base.Add(60 * time.Second), cpuJiffies: 10, rdBytes: 0}

	if _, ok := buildBatch(meta, "", prev, cur); ok {
		t.Error("expected regression to suppress the batch")
	}
}

func TestBuildBatchNoBalloon(t *testing.T) {
	meta := types.GuestTelemetryMeta{InstanceID: "i-0abc", VCPUs: 1, PeriodSeconds: 60}
	base := time.Unix(1000, 0)
	batch, ok := buildBatch(meta, "",
		&sample{at: base}, &sample{at: base.Add(time.Minute)})
	if !ok {
		t.Fatal("expected batch")
	}
	for _, s := range batch.Series {
		if s.Name == "goanna_ec2_memory_actual_bytes" {
			t.Error("memory series emitted without balloon")
		}
		if _, ok := s.Labels["account_id"]; ok {
			t.Error("empty account_id must be omitted")
		}
	}
}

func TestPollerPeriod(t *testing.T) {
	tests := []struct {
		secs int
		want time.Duration
	}{
		{60, 60 * time.Second},
		{300, 300 * time.Second},
		{0, 300 * time.Second},  // absent -> basic
		{17, 300 * time.Second}, // garbage -> basic
	}
	for _, tt := range tests {
		p := newPoller(&Config{}, nil, types.GuestTelemetryMeta{PeriodSeconds: tt.secs})
		if got := p.period(); got != tt.want {
			t.Errorf("period(%d) = %v, want %v", tt.secs, got, tt.want)
		}
	}
}

func TestStaggerOffset(t *testing.T) {
	period := 60 * time.Second
	a := staggerOffset("i-0aaa", period)
	b := staggerOffset("i-0aaa", period)
	if a != b {
		t.Error("stagger must be deterministic")
	}
	if a < 0 || a >= period {
		t.Errorf("offset %v outside [0, %v)", a, period)
	}
}
