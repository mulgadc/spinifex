package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/systemd"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpgradeSeesEveryConfigTarget pins the registrations `spx admin upgrade`
// depends on. A target whose package is no longer linked here would leave the
// command reporting nothing at all for a config file it installs.
//
// spinifex.toml is the only config target: predastore.toml is not migrated at
// all, so a predastore config on disk must contribute neither a version nor a
// pending step. No migrations are registered, so nothing is ever pending.
func TestUpgradeSeesEveryConfigTarget(t *testing.T) {
	configDir := t.TempDir()

	// A target must exist on disk to be reported at all.
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "predastore"), 0750))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "spinifex.toml"), []byte("version = \"4\"\n"), 0640))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "predastore", "predastore.toml"), []byte("version = 1\n"), 0640))

	versions, err := migrate.DefaultRegistry.ConfigVersions(configDir)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"spinifex.toml": 4}, versions)

	pending, err := migrate.DefaultRegistry.PendingConfig(configDir)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestValidateUpgradeFlags(t *testing.T) {
	assert.NoError(t, validateUpgradeFlags(false, false))
	assert.NoError(t, validateUpgradeFlags(true, false))
	assert.NoError(t, validateUpgradeFlags(false, true))
	assert.ErrorIs(t, validateUpgradeFlags(true, true), errUpgradeFlagsConflict)
}

func TestUpgradeSummary(t *testing.T) {
	cases := []struct {
		name                       string
		dryRun, hasConfig, hasUnit bool
		wantProceed                bool
		wantMessage                string
	}{
		{"dry-run, nothing pending", true, false, false, false, "\nNothing pending — config and units are current."},
		{"dry-run, config pending", true, true, false, false, ""},
		{"dry-run, unit pending", true, false, true, false, ""},
		{"dry-run, both pending", true, true, true, false, ""},
		{"apply, nothing pending", false, false, false, false, "\nNothing to do."},
		{"apply, config pending", false, true, false, true, ""},
		{"apply, unit pending", false, false, true, true, ""},
		{"apply, both pending", false, true, true, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proceed, message := upgradeSummary(tc.dryRun, tc.hasConfig, tc.hasUnit)
			assert.Equal(t, tc.wantProceed, proceed)
			assert.Equal(t, tc.wantMessage, message)
		})
	}
}

func TestReportUnitStatus_PrintsEachActionAndConflictFooter(t *testing.T) {
	result := systemd.Result{Statuses: []systemd.UnitStatus{
		{Name: "noop.service", Action: systemd.ActionNoop, InstalledVersion: 3},
		{Name: "install.service", Action: systemd.ActionInstall, EmbeddedVersion: 2},
		{Name: "replace.service", Action: systemd.ActionReplace, InstalledVersion: 1, EmbeddedVersion: 2},
		{Name: "conflict.service", Action: systemd.ActionConflict, InstalledVersion: 2},
	}}

	out := captureStdout(t, func() { reportUnitStatus(result) })

	for _, want := range []string{
		"noop.service:", "3 (up to date)",
		"install.service:", "missing → will install (v2)",
		"replace.service:", "1 → 2 (stale, will replace)",
		"conflict.service:", "2 (operator-modified, not touched",
		"Operator-modified units above are left as-is",
	} {
		assert.Contains(t, out, want)
	}
}

func TestReportUnitStatus_NoConflictsOmitsFooter(t *testing.T) {
	result := systemd.Result{Statuses: []systemd.UnitStatus{{Name: "a.service", Action: systemd.ActionNoop}}}
	out := captureStdout(t, func() { reportUnitStatus(result) })
	assert.NotContains(t, out, "operator-modified")
}

func TestReportUnitApply_PrintsAppliedUnitsAndReload(t *testing.T) {
	result := systemd.Result{
		ReloadRan: true,
		Statuses: []systemd.UnitStatus{
			{Name: "skipped.service", Applied: false},
			{Name: "installed.service", Applied: true},
			{Name: "replaced.service", Applied: true, Backup: "/etc/systemd/system/replaced.service.bak"},
		},
	}
	out := captureStdout(t, func() { reportUnitApply(result) })

	assert.NotContains(t, out, "skipped.service", "unapplied unit must not be printed")
	for _, want := range []string{
		"installed.service installed",
		"replaced.service replaced (backup: /etc/systemd/system/replaced.service.bak)",
		"systemctl daemon-reload complete.",
	} {
		assert.Contains(t, out, want)
	}
}

func TestReportUnitApply_NoReloadOmitsMessage(t *testing.T) {
	out := captureStdout(t, func() { reportUnitApply(systemd.Result{}) })
	assert.NotContains(t, out, "daemon-reload")
}

func TestReportConfigStatus_NoPendingMigrations(t *testing.T) {
	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "spinifex.toml"), []byte("version = \"4\"\n"), 0640))

	var pending []migrate.PendingMigration
	out := captureStdout(t, func() { pending = reportConfigStatus(configDir) })

	assert.Empty(t, pending)
	assert.Contains(t, out, "No pending config migrations.")
	assert.Contains(t, out, "spinifex.toml:")
}

// captureStderr redirects os.Stderr for the duration of fn and returns what
// was written. runAdminUpgrade's error paths print to stderr, not stdout.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stderr
	os.Stderr = w //nolint:reassign // test-local stderr capture, restored below before this func returns

	fn()

	require.NoError(t, w.Close())
	os.Stderr = orig //nolint:reassign // restoring the real os.Stderr captured above
	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)
	return buf.String()
}

// upgradeExitPanic is the sentinel withUpgradeExitCapture's fake upgradeExit
// panics with, so a deferred recover can distinguish "the command called
// exit" from any other panic escaping fn.
type upgradeExitPanic struct{ code int }

// withUpgradeExitCapture swaps upgradeExit for a fake that records the exit
// code and panics with upgradeExitPanic instead of killing the test binary,
// runs fn, and returns the recorded code (-1 if fn returned without
// exiting). This lets runAdminUpgrade's real control flow, including its
// error/exit branches, be exercised directly.
func withUpgradeExitCapture(t *testing.T, fn func()) int {
	t.Helper()
	origExit := upgradeExit
	t.Cleanup(func() { upgradeExit = origExit })

	code := -1
	upgradeExit = func(c int) { code = c; panic(upgradeExitPanic{c}) }

	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(upgradeExitPanic); !ok {
					panic(r)
				}
			}
		}()
		fn()
	}()
	return code
}

// newUpgradeTestCmd builds an isolated command tree mirroring rootCmd's and
// upgradeCmd's flag wiring, so runAdminUpgrade can be driven directly
// without touching the package's real global command tree (which other
// tests in this package also mutate).
func newUpgradeTestCmd(t *testing.T, configDir, dataDir string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "spx"}
	root.PersistentFlags().String("config-dir", configDir, "")
	root.PersistentFlags().String("spinifex-dir", dataDir, "")

	upgrade := &cobra.Command{Use: "upgrade", Run: runAdminUpgrade}
	upgrade.Flags().Bool("yes", false, "")
	upgrade.Flags().Bool("dry-run", false, "")
	upgrade.Flags().Bool("units-only", false, "")
	upgrade.Flags().Bool("skip-units", false, "")
	root.AddCommand(upgrade)

	// runAdminUpgrade reads config-dir/spinifex-dir via cmd.Root().Flags(),
	// which only sees persistent flags after they've been merged in —
	// normally done by Execute(). Parsing with no args merges them without
	// running the command.
	require.NoError(t, root.ParseFlags(nil))
	return upgrade
}

// setUpgradeFlags sets each flag=value pair on cmd, failing the test on any
// unknown flag or parse error.
func setUpgradeFlags(t *testing.T, cmd *cobra.Command, flags map[string]string) {
	t.Helper()
	for name, value := range flags {
		require.NoError(t, cmd.Flags().Set(name, value))
	}
}

// seedUnitRoot writes every embedded unit unmodified into dir, so a
// Reconcile pass against dir reports every unit ActionNoop.
func seedUnitRoot(t *testing.T, dir string) {
	t.Helper()
	for name, content := range systemd.Units {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0644))
	}
}

// withUnitRoot points the package's systemdUnitRoot var at dir for the
// duration of the test, restoring the original on cleanup.
func withUnitRoot(t *testing.T, dir string) {
	t.Helper()
	prev := systemdUnitRoot
	systemdUnitRoot = dir
	t.Cleanup(func() { systemdUnitRoot = prev })
}

func TestRunAdminUpgrade_MutuallyExclusiveFlagsExits(t *testing.T) {
	cmd := newUpgradeTestCmd(t, t.TempDir(), t.TempDir())
	setUpgradeFlags(t, cmd, map[string]string{"units-only": "true", "skip-units": "true"})

	var code int
	out := captureStdout(t, func() {
		code = withUpgradeExitCapture(t, func() { runAdminUpgrade(cmd, nil) })
	})
	assert.Equal(t, 1, code)
	assert.Empty(t, out, "no stdout expected before the mutual-exclusivity check exits")
}

func TestRunAdminUpgrade_MissingInstallationExits(t *testing.T) {
	configDir := t.TempDir() // no spinifex.toml written
	cmd := newUpgradeTestCmd(t, configDir, t.TempDir())

	code := withUpgradeExitCapture(t, func() { runAdminUpgrade(cmd, nil) })
	assert.Equal(t, 1, code)
}

func TestRunAdminUpgrade_DryRunNothingPending(t *testing.T) {
	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "spinifex.toml"), []byte("version = \"4\"\n"), 0640))
	unitRoot := t.TempDir()
	seedUnitRoot(t, unitRoot)
	withUnitRoot(t, unitRoot)

	cmd := newUpgradeTestCmd(t, configDir, t.TempDir())
	setUpgradeFlags(t, cmd, map[string]string{"dry-run": "true"})

	var code int
	out := captureStdout(t, func() {
		code = withUpgradeExitCapture(t, func() { runAdminUpgrade(cmd, nil) })
	})
	assert.Equal(t, -1, code, "must not exit when everything is current")
	assert.Contains(t, out, "Nothing pending — config and units are current.")
}

func TestRunAdminUpgrade_DryRunReportsUnitsPending(t *testing.T) {
	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "spinifex.toml"), []byte("version = \"4\"\n"), 0640))
	withUnitRoot(t, t.TempDir()) // empty: every unit reports missing

	cmd := newUpgradeTestCmd(t, configDir, t.TempDir())
	setUpgradeFlags(t, cmd, map[string]string{"dry-run": "true"})

	var code int
	out := captureStdout(t, func() {
		code = withUpgradeExitCapture(t, func() { runAdminUpgrade(cmd, nil) })
	})
	assert.Equal(t, -1, code)
	assert.NotContains(t, out, "Nothing pending")
	assert.Contains(t, out, "will install")
}

func TestRunAdminUpgrade_UnitsOnlySkipsConfigReporting(t *testing.T) {
	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "spinifex.toml"), []byte("version = \"4\"\n"), 0640))
	withUnitRoot(t, t.TempDir())

	cmd := newUpgradeTestCmd(t, configDir, t.TempDir())
	setUpgradeFlags(t, cmd, map[string]string{"dry-run": "true", "units-only": "true"})

	var code int
	out := captureStdout(t, func() {
		code = withUpgradeExitCapture(t, func() { runAdminUpgrade(cmd, nil) })
	})
	assert.Equal(t, -1, code)
	assert.NotContains(t, out, "Reading current config versions", "--units-only must skip config reporting")
	assert.Contains(t, out, "Reading installed systemd unit versions")
}

func TestRunAdminUpgrade_SkipUnitsSkipsUnitReporting(t *testing.T) {
	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "spinifex.toml"), []byte("version = \"4\"\n"), 0640))
	// Point systemdUnitRoot at a path Reconcile must never touch: if
	// --skip-units leaked a call through, a nonexistent root would still
	// "work" for a dry run, so the real proof is that its output never
	// appears at all.
	withUnitRoot(t, filepath.Join(t.TempDir(), "never-touched"))

	cmd := newUpgradeTestCmd(t, configDir, t.TempDir())
	setUpgradeFlags(t, cmd, map[string]string{"dry-run": "true", "skip-units": "true"})

	var code int
	out := captureStdout(t, func() {
		code = withUpgradeExitCapture(t, func() { runAdminUpgrade(cmd, nil) })
	})
	assert.Equal(t, -1, code)
	assert.NotContains(t, out, "Reading installed systemd unit versions", "--skip-units must skip unit reporting")
	assert.Contains(t, out, "Reading current config versions")
}

func TestRunAdminUpgrade_ApplyWithNonWritableUnitRootExitsWithRootHint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permissions; this test needs an unprivileged process")
	}
	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "spinifex.toml"), []byte("version = \"4\"\n"), 0640))

	unitRoot := t.TempDir() // left empty: every unit reports missing
	require.NoError(t, os.Chmod(unitRoot, 0o500))
	t.Cleanup(func() { _ = os.Chmod(unitRoot, 0o700) })
	withUnitRoot(t, unitRoot)

	cmd := newUpgradeTestCmd(t, configDir, t.TempDir())
	setUpgradeFlags(t, cmd, map[string]string{"yes": "true"})

	var code int
	var errOut string
	captureStdout(t, func() {
		errOut = captureStderr(t, func() {
			code = withUpgradeExitCapture(t, func() { runAdminUpgrade(cmd, nil) })
		})
	})
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "Unit reconciliation failed")
	assert.Contains(t, errOut, "Re-run as root to apply the unit changes reported above")
}

func TestRunAdminUpgrade_DryRunReconcileErrorExits(t *testing.T) {
	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "spinifex.toml"), []byte("version = \"4\"\n"), 0640))

	unitRoot := t.TempDir()
	// A directory in place of a unit file fails the read with something
	// other than IsNotExist, so the very first (dry-run) Reconcile call
	// returns an error.
	var oneName string
	for name := range systemd.Units {
		oneName = name
		break
	}
	require.NoError(t, os.Mkdir(filepath.Join(unitRoot, oneName), 0o755))
	withUnitRoot(t, unitRoot)

	cmd := newUpgradeTestCmd(t, configDir, t.TempDir())

	var code int
	var errOut string
	captureStdout(t, func() {
		errOut = captureStderr(t, func() {
			code = withUpgradeExitCapture(t, func() { runAdminUpgrade(cmd, nil) })
		})
	})
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut, "Error checking systemd units")
}
