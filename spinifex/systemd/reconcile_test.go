package systemd

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// realUnit returns the embedded content for a real production unit, so
// reconcile tests exercise the actual shipped units rather than fixtures
// that could drift from what Reconcile runs against in production.
func realUnit(t *testing.T, name string) string {
	t.Helper()
	content, ok := Units[name]
	if !ok {
		t.Fatalf("no embedded unit %q", name)
	}
	return content
}

// stripFirstLine drops a unit's version-marker line, simulating an
// installed copy from before units were versioned.
func stripFirstLine(content string) string {
	_, rest, _ := strings.Cut(content, "\n")
	return rest
}

// setVersion rewrites a unit's version-marker line to v, keeping the body
// unchanged — simulating an installed copy stamped at an older version.
func setVersion(content string, v int) string {
	return "# spinifex-unit-version: " + strconv.Itoa(v) + "\n" + stripFirstLine(content)
}

func findStatus(t *testing.T, result Result, name string) UnitStatus {
	t.Helper()
	for _, s := range result.Statuses {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no status for %s", name)
	return UnitStatus{}
}

func stubDaemonReload(t *testing.T) func() int {
	t.Helper()
	calls := 0
	prev := systemctlDaemonReload
	systemctlDaemonReload = func() error {
		calls++
		return nil
	}
	t.Cleanup(func() { systemctlDaemonReload = prev })
	return func() int { return calls }
}

func TestReconcile_MissingUnitIsInstalled(t *testing.T) {
	root := t.TempDir()
	reloadCalls := stubDaemonReload(t)
	const name = "spinifex-ui.service"
	embedded := realUnit(t, name)

	result, err := Reconcile(root, Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	st := findStatus(t, result, name)
	if st.Action != ActionInstall {
		t.Errorf("Action = %s, want %s", st.Action, ActionInstall)
	}
	if !st.Applied {
		t.Error("Applied = false, want true")
	}
	if st.Backup != "" {
		t.Errorf("Backup = %q, want empty — nothing existed to back up", st.Backup)
	}

	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("read installed unit: %v", err)
	}
	if string(got) != embedded {
		t.Error("installed content does not match embedded unit")
	}
	if reloadCalls() != 1 {
		t.Errorf("daemon-reload calls = %d, want 1", reloadCalls())
	}
}

func TestReconcile_OlderVersionIsReplacedWithBackup(t *testing.T) {
	root := t.TempDir()
	stubDaemonReload(t)
	const name = "spinifex-daemon.service"
	embedded := realUnit(t, name)
	stale := setVersion(embedded, 0)
	writeFile(t, root, name, stale)

	result, err := Reconcile(root, Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	st := findStatus(t, result, name)
	if st.Action != ActionReplace {
		t.Errorf("Action = %s, want %s", st.Action, ActionReplace)
	}
	if st.InstalledVersion != 0 {
		t.Errorf("InstalledVersion = %d, want 0", st.InstalledVersion)
	}
	if st.Backup == "" {
		t.Fatal("Backup path empty, want a timestamped backup")
	}

	backupContent, err := os.ReadFile(st.Backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backupContent) != stale {
		t.Error("backup content does not match the pre-replace installed content")
	}

	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("read replaced unit: %v", err)
	}
	if string(got) != embedded {
		t.Error("replaced content does not match embedded unit")
	}
}

func TestReconcile_NoMarkerIsTreatedAsVersionZeroAndReplaced(t *testing.T) {
	root := t.TempDir()
	stubDaemonReload(t)
	const name = "spinifex-viperblock.service"
	embedded := realUnit(t, name)
	noMarker := stripFirstLine(embedded)
	writeFile(t, root, name, noMarker)

	result, err := Reconcile(root, Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	st := findStatus(t, result, name)
	if st.Action != ActionReplace {
		t.Errorf("Action = %s, want %s", st.Action, ActionReplace)
	}
	if st.InstalledVersion != 0 {
		t.Errorf("InstalledVersion = %d, want 0 (absent marker)", st.InstalledVersion)
	}
	if st.Backup == "" {
		t.Error("Backup path empty, want a timestamped backup")
	}
}

func TestReconcile_IdenticalContentIsNoop(t *testing.T) {
	root := t.TempDir()
	reloadCalls := stubDaemonReload(t)
	// Seed every embedded unit as already-current, so the only thing left
	// for Reconcile to decide on is the one unit under test — otherwise the
	// other 15 real units would still be "missing" and trigger a reload.
	seedAllUnits(t, root)
	const name = "spinifex-northstar.service"

	result, err := Reconcile(root, Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	st := findStatus(t, result, name)
	if st.Action != ActionNoop {
		t.Errorf("Action = %s, want %s", st.Action, ActionNoop)
	}
	if st.Applied {
		t.Error("Applied = true, want false — nothing should change")
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "pre-reconcile") {
			t.Errorf("unexpected backup file for a no-op unit: %s", e.Name())
		}
	}
	if reloadCalls() != 0 {
		t.Errorf("daemon-reload calls = %d, want 0 — nothing changed", reloadCalls())
	}
}

func TestReconcile_OperatorModifiedAtCurrentVersionIsLeftAlone(t *testing.T) {
	root := t.TempDir()
	stubDaemonReload(t)
	const name = "spinifex-vpcd.service"
	embedded := realUnit(t, name)
	modified := embedded + "# operator hand-edit, not from the shipped unit\n"
	writeFile(t, root, name, modified)

	result, err := Reconcile(root, Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	st := findStatus(t, result, name)
	if st.Action != ActionConflict {
		t.Errorf("Action = %s, want %s", st.Action, ActionConflict)
	}
	if st.Applied {
		t.Error("Applied = true, want false — a same-version conflict must never be overwritten")
	}
	if st.Backup != "" {
		t.Errorf("Backup = %q, want empty — nothing was written", st.Backup)
	}

	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != modified {
		t.Error("operator-modified unit must be untouched byte-for-byte")
	}
	if !result.HasConflicts() {
		t.Error("HasConflicts() = false, want true")
	}
}

func TestReconcile_DryRunChangesNothing(t *testing.T) {
	root := t.TempDir()
	reloadCalls := stubDaemonReload(t)
	const staleName = "spinifex-awsgw.service"
	stale := setVersion(realUnit(t, staleName), 0)
	writeFile(t, root, staleName, stale)

	result, err := Reconcile(root, Options{DryRun: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.HasChanges() {
		t.Fatal("HasChanges() = false, want true — a stale unit and missing units are pending")
	}

	for _, s := range result.Statuses {
		if s.Applied {
			t.Errorf("%s: Applied = true during a dry run", s.Name)
		}
	}

	got, err := os.ReadFile(filepath.Join(root, staleName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != stale {
		t.Error("dry run must not modify the stale unit on disk")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("dry run wrote %d files to root, want exactly the 1 pre-seeded file", len(entries))
	}
	if reloadCalls() != 0 {
		t.Errorf("daemon-reload calls = %d, want 0 during a dry run", reloadCalls())
	}
}

func TestReconcile_NonWritableRootReportsWithoutPartialWrites(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions; this test needs an unprivileged process")
	}

	root := t.TempDir()
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	result, err := Reconcile(root, Options{})
	if err == nil {
		t.Fatal("Reconcile: want an error on a non-writable root, got nil")
	}
	if !errors.Is(err, ErrRootRequired) {
		t.Errorf("err = %v, want it to wrap ErrRootRequired", err)
	}
	if len(result.Statuses) == 0 {
		t.Error("Statuses empty — drift must still be reported even when it cannot be applied")
	}
	for _, s := range result.Statuses {
		if s.Applied {
			t.Errorf("%s: Applied = true despite a non-writable root", s.Name)
		}
	}
}

func TestReconcile_ReloadRunsOnceThenSettles(t *testing.T) {
	root := t.TempDir()
	reloadCalls := stubDaemonReload(t)

	if _, err := Reconcile(root, Options{}); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if reloadCalls() != 1 {
		t.Fatalf("daemon-reload calls after first apply = %d, want 1", reloadCalls())
	}

	result, err := Reconcile(root, Options{})
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if result.HasChanges() {
		t.Error("HasChanges() = true on a second pass over an already-reconciled root")
	}
	if reloadCalls() != 1 {
		t.Errorf("daemon-reload calls after idempotent second apply = %d, want still 1", reloadCalls())
	}
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedAllUnits pre-populates root with every embedded unit at its current
// content, so a Reconcile pass has nothing pending except what the test
// deliberately alters afterward.
func seedAllUnits(t *testing.T, root string) {
	t.Helper()
	for name, content := range Units {
		writeFile(t, root, name, content)
	}
}
