package qmpcollector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	telemetryv1 "github.com/mulgadc/spinifex/contracts/telemetry/v1"
)

func writeMeta(t *testing.T, dir string, meta telemetryv1.GuestTelemetryMeta) string {
	t.Helper()
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	path := telemetryv1.GuestTelemetryMetaPath(dir, meta.InstanceID)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReconcile(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{RuntimeDir: dir, ProcRoot: dir, SysRoot: dir,
		DiscoverInterval: time.Hour}
	c := newCollector(cfg, nil)
	ctx := t.Context()

	// Live VM: metadata + socket present.
	liveSock := filepath.Join(dir, telemetryv1.QMPTelemetryPrefix+"i-live.sock")
	touch(t, liveSock)
	writeMeta(t, dir, telemetryv1.GuestTelemetryMeta{
		InstanceID: "i-live", Socket: liveSock, PeriodSeconds: 300})

	// Stale VM: metadata but no socket (unclean QEMU exit) — must be GC'd.
	stalePath := writeMeta(t, dir, telemetryv1.GuestTelemetryMeta{
		InstanceID: "i-stale", Socket: filepath.Join(dir, "gone.sock"), PeriodSeconds: 300})

	// Garbage file matching the glob — skipped, not fatal.
	garbage := telemetryv1.GuestTelemetryMetaPath(dir, "i-bad")
	if err := os.WriteFile(garbage, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	c.reconcile(ctx)

	if _, ok := c.pollers["i-live"]; !ok {
		t.Error("expected poller for i-live")
	}
	if _, ok := c.pollers["i-stale"]; ok {
		t.Error("stale instance must not get a poller")
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Error("stale metadata must be removed")
	}
	if len(c.pollers) != 1 {
		t.Errorf("pollers = %d, want 1", len(c.pollers))
	}

	// Metadata refresh (ENI hotplug) must update the running poller in place.
	writeMeta(t, dir, telemetryv1.GuestTelemetryMeta{
		InstanceID: "i-live", Socket: liveSock, PeriodSeconds: 300,
		Taps: []string{"tapnew"}})
	c.reconcile(ctx)
	if got := c.pollers["i-live"].snapshotMeta().Taps; len(got) != 1 || got[0] != "tapnew" {
		t.Errorf("taps after refresh = %v, want [tapnew]", got)
	}

	// VM gone: metadata removed — poller must stop and be dropped.
	if err := os.Remove(telemetryv1.GuestTelemetryMetaPath(dir, "i-live")); err != nil {
		t.Fatal(err)
	}
	c.reconcile(ctx)
	if len(c.pollers) != 0 {
		t.Errorf("pollers after removal = %d, want 0", len(c.pollers))
	}
}

func TestLabelKey(t *testing.T) {
	a := labelKey("m", map[string]string{"b": "2", "a": "1"})
	b := labelKey("m", map[string]string{"a": "1", "b": "2"})
	if a != b {
		t.Errorf("label key must be order-independent: %q vs %q", a, b)
	}
	if c := labelKey("m", map[string]string{"a": "1", "b": "3"}); c == a {
		t.Error("different values must produce different keys")
	}
}
