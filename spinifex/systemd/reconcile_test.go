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

// withFakeUnit temporarily adds name to the package-level Units map, so a
// test can control the exact path Reconcile computes (including a name with
// a path separator, to point at a directory that does or doesn't exist)
// without touching the generated production unit set.
func withFakeUnit(t *testing.T, name, content string) {
	t.Helper()
	prev, existed := Units[name]
	Units[name] = content
	t.Cleanup(func() {
		if existed {
			Units[name] = prev
		} else {
			delete(Units, name)
		}
	})
}

func TestHasConflicts_NoConflictsIsFalse(t *testing.T) {
	result := Result{Statuses: []UnitStatus{
		{Name: "a.service", Action: ActionNoop},
		{Name: "b.service", Action: ActionInstall},
	}}
	if result.HasConflicts() {
		t.Error("HasConflicts() = true, want false — no unit is ActionConflict")
	}
}

func TestUnitVersion_OverflowingMarkerIsVersionZero(t *testing.T) {
	// A marker value too large for strconv.Atoi must be treated the same as
	// no marker at all, not propagate the parse error.
	content := "# spinifex-unit-version: 99999999999999999999999999\nExecStart=/usr/bin/true\n"
	if v := unitVersion(content); v != 0 {
		t.Errorf("unitVersion() = %d, want 0 for an overflowing marker value", v)
	}
}

func TestUnitVersion_MarkerNotOnFirstLineIsIgnored(t *testing.T) {
	content := "[Unit]\n# spinifex-unit-version: 5\nExecStart=/usr/bin/true\n"
	if v := unitVersion(content); v != 0 {
		t.Errorf("unitVersion() = %d, want 0 — a marker must be on the first line to count", v)
	}
}

func TestSystemctlDaemonReload_MissingBinaryReturnsError(t *testing.T) {
	// Exercises the real (non-stubbed) production implementation: an empty
	// PATH means systemctl cannot be found, so the command fails cleanly
	// instead of touching a real systemd instance.
	t.Setenv("PATH", t.TempDir())
	if err := systemctlDaemonReload(); err == nil {
		t.Fatal("systemctlDaemonReload(): want an error when systemctl is not on PATH, got nil")
	}
}

func TestReconcile_UnreadableInstalledUnitReturnsError(t *testing.T) {
	root := t.TempDir()
	stubDaemonReload(t)
	const name = "spinifex-ui.service"
	// A directory in place of the unit file fails the read with something
	// other than IsNotExist, exercising the general read-error path
	// distinct from "missing unit".
	if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Reconcile(root, Options{})
	if err == nil {
		t.Fatal("Reconcile: want an error when an installed unit path is unreadable, got nil")
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("err = %v, want it to name the unreadable unit %s", err, name)
	}
}

func TestReconcile_WriteFailureIsSurfaced(t *testing.T) {
	root := t.TempDir()
	stubDaemonReload(t)

	// A unit whose path has a parent directory that does not exist: the
	// read reports NotExist (classified Install, like any missing unit),
	// but the later write into that missing directory fails. Deterministic
	// and root-safe, unlike a permission-based probe.
	const name = "missing-parent/fake-install.service"
	withFakeUnit(t, name, "# spinifex-unit-version: 1\nExecStart=/usr/bin/true\n")

	_, err := Reconcile(root, Options{})
	if err == nil {
		t.Fatal("Reconcile: want an error when the write's parent directory is missing, got nil")
	}
	if !strings.Contains(err.Error(), "write") {
		t.Errorf("err = %v, want it to mention the write step", err)
	}
}

func TestReconcile_BackupFailureIsSurfaced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions; this test needs an unprivileged process")
	}
	root := t.TempDir()
	stubDaemonReload(t)

	const name = "nested/fake-backup.service"
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	embedded := "# spinifex-unit-version: 2\nExecStart=/usr/bin/true\n"
	withFakeUnit(t, name, embedded)
	stale := setVersion(embedded, 0)
	writeFile(t, root, name, stale)

	// Lock the nested directory only after seeding it: the initial read
	// (classification) still succeeds, but backupUnit's write of the
	// backup copy into the same directory fails.
	if err := os.Chmod(nested, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(nested, 0o755) })

	_, err := Reconcile(root, Options{})
	if err == nil {
		t.Fatal("Reconcile: want an error when the backup write fails, got nil")
	}
	if !strings.Contains(err.Error(), "backup") {
		t.Errorf("err = %v, want it to mention the backup step", err)
	}
}

func TestReconcile_DaemonReloadFailureIsSurfaced(t *testing.T) {
	root := t.TempDir()
	prevReload := systemctlDaemonReload
	wantErr := errors.New("boom: reload failed")
	systemctlDaemonReload = func() error { return wantErr }
	t.Cleanup(func() { systemctlDaemonReload = prevReload })

	result, err := Reconcile(root, Options{})
	if err == nil {
		t.Fatal("Reconcile: want an error when daemon-reload fails, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want it to wrap %v", err, wantErr)
	}
	if result.ReloadRan {
		t.Error("ReloadRan = true despite daemon-reload failing")
	}
}

func TestReconcile_DowngradeMarkerIsConflictNotClobbered(t *testing.T) {
	root := t.TempDir()
	stubDaemonReload(t)
	const name = "spinifex-vpcd.service"
	embedded := realUnit(t, name)
	// Simulate a downgrade: the installed unit is stamped at a version
	// newer than what this binary embeds. Reconcile must never clobber a
	// newer file with an older one — ActionReplace only ever moves forward.
	future := setVersion(embedded, unitVersion(embedded)+1) + "# from a future release\n"
	writeFile(t, root, name, future)

	result, err := Reconcile(root, Options{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	st := findStatus(t, result, name)
	if st.Action != ActionConflict {
		t.Errorf("Action = %s, want %s — a newer installed marker must never be replaced", st.Action, ActionConflict)
	}
	if st.Applied {
		t.Error("Applied = true, want false — a downgrade must not clobber a newer unit")
	}
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != future {
		t.Error("downgrade case: installed unit content must be untouched")
	}
}

func TestBackupUnit_UnreadableSourceReturnsError(t *testing.T) {
	_, err := backupUnit(filepath.Join(t.TempDir(), "does-not-exist.service"), 0, 1)
	if err == nil {
		t.Fatal("backupUnit: want an error for a nonexistent source path, got nil")
	}
}
