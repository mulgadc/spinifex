package qmp

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
)

// Recorded QMP return payloads (QEMU 9.x) for the telemetry queries.
const (
	recordedCPUsFast = `[
		{"thread-id": 34237, "props": {"core-id": 0, "thread-id": 0, "socket-id": 0}, "qom-path": "/machine/unattached/device[0]", "cpu-index": 0, "target": "x86_64"},
		{"thread-id": 34238, "props": {"core-id": 1, "thread-id": 0, "socket-id": 0}, "qom-path": "/machine/unattached/device[2]", "cpu-index": 1, "target": "x86_64"}
	]`
	recordedBlockStats = `[
		{"device": "", "node-name": "vol-1", "stats": {"flush_total_time_ns": 1000, "wr_highest_offset": 8192, "wr_total_time_ns": 5000, "failed_wr_operations": 0, "failed_rd_operations": 0, "wr_merged": 0, "wr_bytes": 4096, "timed_stats": [], "failed_unmap_operations": 0, "failed_flush_operations": 0, "account_invalid": true, "rd_total_time_ns": 3000, "invalid_unmap_operations": 0, "flush_operations": 3, "wr_operations": 7, "unmap_merged": 0, "rd_merged": 0, "rd_bytes": 123456, "invalid_flush_operations": 0, "account_failed": true, "idle_time_ns": 500, "rd_operations": 42, "invalid_wr_operations": 0, "invalid_rd_operations": 0, "unmap_bytes": 0, "unmap_operations": 0, "unmap_total_time_ns": 0}},
		{"device": "ide1-cd0", "stats": {"rd_bytes": 100, "wr_bytes": 0, "rd_operations": 5, "wr_operations": 0}}
	]`
	recordedBalloon = `{"actual": 1073741824}`
)

func TestTelemetryResponseParsing(t *testing.T) {
	t.Run("query-cpus-fast", func(t *testing.T) {
		var cpus []CPUInfoFast
		if err := json.Unmarshal([]byte(recordedCPUsFast), &cpus); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(cpus) != 2 {
			t.Fatalf("got %d vCPUs, want 2", len(cpus))
		}
		if cpus[0].ThreadID != 34237 || cpus[0].CPUIndex != 0 {
			t.Errorf("cpu[0] = %+v, want thread 34237 index 0", cpus[0])
		}
		if cpus[1].ThreadID != 34238 || cpus[1].CPUIndex != 1 {
			t.Errorf("cpu[1] = %+v, want thread 34238 index 1", cpus[1])
		}
	})

	t.Run("query-blockstats", func(t *testing.T) {
		var stats []BlockStats
		if err := json.Unmarshal([]byte(recordedBlockStats), &stats); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(stats) != 2 {
			t.Fatalf("got %d devices, want 2", len(stats))
		}
		s := stats[0].Stats
		if s.RdBytes != 123456 || s.WrBytes != 4096 || s.RdOperations != 42 || s.WrOperations != 7 {
			t.Errorf("stats[0] = %+v", s)
		}
	})

	t.Run("query-balloon", func(t *testing.T) {
		var b BalloonInfo
		if err := json.Unmarshal([]byte(recordedBalloon), &b); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if b.Actual != 1073741824 {
			t.Errorf("actual = %d, want 1073741824", b.Actual)
		}
	})
}

// fakeQMPServer speaks just enough QMP for one client: greeting, then a fixed
// response per command, with an async event injected before the first reply.
func fakeQMPServer(t *testing.T, responses map[string]string) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "qmp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte(`{"QMP": {"version": {"qemu": {"major": 9, "minor": 2}}, "capabilities": []}}` + "\n"))
		dec := json.NewDecoder(conn)
		first := true
		for {
			var cmd QMPCommand
			if err := dec.Decode(&cmd); err != nil {
				return
			}
			if first {
				first = false
				_, _ = conn.Write([]byte(`{"event": "NIC_RX_FILTER_CHANGED", "timestamp": {"seconds": 1, "microseconds": 2}}` + "\n"))
			}
			resp, ok := responses[cmd.Execute]
			if !ok {
				resp = `{"error": {"class": "CommandNotFound", "desc": "not found"}}`
			}
			_, _ = conn.Write([]byte(resp + "\n"))
		}
	}()
	return sock
}

func TestClientExecute(t *testing.T) {
	sock := fakeQMPServer(t, map[string]string{
		"qmp_capabilities": `{"return": {}}`,
		"query-cpus-fast":  `{"return": ` + recordedCPUsFast + `}`,
		"query-balloon":    `{"return": ` + recordedBalloon + `}`,
	})

	client, err := NewQMPClient(sock)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = client.Close() }()

	// Event interleaved before the capabilities reply must be skipped.
	if err := client.Execute(QMPCommand{Execute: "qmp_capabilities"}, nil); err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	var cpus []CPUInfoFast
	if err := client.Execute(QMPCommand{Execute: "query-cpus-fast"}, &cpus); err != nil {
		t.Fatalf("query-cpus-fast: %v", err)
	}
	if len(cpus) != 2 || cpus[1].ThreadID != 34238 {
		t.Errorf("cpus = %+v", cpus)
	}
	var balloon BalloonInfo
	if err := client.Execute(QMPCommand{Execute: "query-balloon"}, &balloon); err != nil {
		t.Fatalf("query-balloon: %v", err)
	}
	if balloon.Actual != 1073741824 {
		t.Errorf("balloon = %+v", balloon)
	}
	// QMP error responses surface as errors.
	if err := client.Execute(QMPCommand{Execute: "query-nonexistent"}, nil); err == nil {
		t.Error("expected error for unknown command")
	}
}
