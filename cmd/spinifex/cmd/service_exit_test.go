package cmd

// Drives the unexported *cobra.Command vars (predastoreStartCmd, etc.)
// directly via RunE, so it must stay in-package.
//test:in-package

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockedDir returns a path whose parent component is a regular file, so
// os.MkdirAll fails with ENOTDIR. Start() calls WritePidFileTo before any
// real infra, so this forces a fast, local, in-process failure there.
func blockedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	return filepath.Join(blocker, "sub")
}

// minimalClusterToml satisfies config.LoadConfig's validation with nothing
// service-specific set, so each case below only has to layer on the one
// field it needs to reach its target branch.
const minimalClusterToml = `
version = "1.0"
epoch = 1
node = "node1"

[nodes.node1]
node = "node1"
region = "us-east-1"
az = "us-east-1a"
`

// writeMalformedToml writes a syntactically invalid TOML file so
// config.LoadConfig fails at viper.ReadInConfig rather than at validation.
func writeMalformedToml(t *testing.T) string {
	t.Helper()
	return writeSpinifexToml(t, "[section\nkey = value without quotes\n")
}

// isolateRuntimeDir redirects the pid-file fallback (utils.pidPath, via
// XDG_RUNTIME_DIR) to an empty temp dir, so a stop command with no explicit
// directory override can't resolve against a real, shared, ambient pid dir.
func isolateRuntimeDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}

// TestServiceStartCmdsReturnErrorOnMissingConfig exercises each service
// start/stop command's RunE directly and asserts a fatal precondition
// failure surfaces as a returned error, not a silent zero-exit return.
// Cases with a setup func drive further into RunE toward a specific branch.
func TestServiceStartCmdsReturnErrorOnMissingConfig(t *testing.T) {
	tests := []struct {
		name    string
		cmd     *cobra.Command
		setup   func(t *testing.T)
		wantErr string
	}{
		{name: "predastore start (missing host-id)", cmd: predastoreStartCmd},
		{
			name: "predastore start (config file load failure)",
			cmd:  predastoreStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeMalformedToml(t))
			},
			wantErr: "load cluster config file",
		},
		{
			name: "predastore start (derive bind failure)",
			cmd:  predastoreStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeSpinifexToml(t, minimalClusterToml))
			},
			wantErr: "derive predastore bind config",
		},
		{
			name: "predastore start (config path not set)",
			cmd:  predastoreStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("predastore-host-id", 1)
			},
			wantErr: "config path is not set",
		},
		{
			name: "predastore start (tls cert not set)",
			cmd:  predastoreStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("predastore-host-id", 1)
				viper.Set("predastore-config-path", filepath.Join(t.TempDir(), "predastore.toml"))
			},
			wantErr: "TLS cert is not set",
		},
		{
			name: "predastore start (tls key not set)",
			cmd:  predastoreStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("predastore-host-id", 1)
				viper.Set("predastore-config-path", filepath.Join(t.TempDir(), "predastore.toml"))
				viper.Set("predastore-tls-cert", filepath.Join(t.TempDir(), "cert.pem"))
			},
			wantErr: "TLS key is not set",
		},
		{
			name: "predastore start (encryption key file not set)",
			cmd:  predastoreStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("predastore-host-id", 1)
				viper.Set("predastore-config-path", filepath.Join(t.TempDir(), "predastore.toml"))
				viper.Set("predastore-tls-cert", filepath.Join(t.TempDir(), "cert.pem"))
				viper.Set("predastore-tls-key", filepath.Join(t.TempDir(), "key.pem"))
			},
			wantErr: "encryption key file is not set",
		},
		{
			name: "predastore start (start failure via unreadable predastore config)",
			cmd:  predastoreStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("predastore-host-id", 1)
				viper.Set("predastore-config-path", filepath.Join(t.TempDir(), "missing-predastore.toml"))
				viper.Set("predastore-tls-cert", filepath.Join(t.TempDir(), "cert.pem"))
				viper.Set("predastore-tls-key", filepath.Join(t.TempDir(), "key.pem"))
				viper.Set("predastore-encryption-key-file", filepath.Join(t.TempDir(), "enc.key"))
				viper.Set("predastore-base-path", t.TempDir())
			},
			wantErr: "start predastore service",
		},
		{name: "viperblock start (missing config)", cmd: viperblockStartCmd},
		{
			name: "viperblock start (config file load failure)",
			cmd:  viperblockStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeMalformedToml(t))
			},
			wantErr: "load config file",
		},
		{
			name: "viperblock start (plugin-path not set)",
			cmd:  viperblockStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeSpinifexToml(t, minimalClusterToml))
			},
			wantErr: "plugin-path must be defined",
		},
		{
			name: "viperblock start (plugin-path does not exist)",
			cmd:  viperblockStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeSpinifexToml(t, minimalClusterToml))
				viper.Set("plugin-path", filepath.Join(t.TempDir(), "missing-plugin.so"))
			},
			wantErr: "plugin-path does not exist",
		},
		{
			name: "viperblock start (start failure via blocked base-dir)",
			cmd:  viperblockStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeSpinifexToml(t, minimalClusterToml))
				viper.Set("plugin-path", t.TempDir())
				viper.Set("base-dir", blockedDir(t))
			},
			wantErr: "start viperblock service",
		},
		{name: "qemunbd start (missing config)", cmd: qemunbdStartCmd},
		{
			name: "qemunbd start (config file load failure)",
			cmd:  qemunbdStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeMalformedToml(t))
			},
			wantErr: "load config file",
		},
		{
			name: "qemunbd start (start failure via blocked base-dir)",
			cmd:  qemunbdStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeSpinifexToml(t, minimalClusterToml))
				viper.Set("base-dir", blockedDir(t))
			},
			wantErr: "start qemunbd service",
		},
		{
			name: "nats start (start failure via blocked data-dir)",
			cmd:  natsStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("data-dir", blockedDir(t))
			},
			wantErr: "start nats service",
		},
		{name: "spinifex start (missing config)", cmd: spinifexStartCmd},
		{
			name: "spinifex start (config file load failure)",
			cmd:  spinifexStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeMalformedToml(t))
			},
			wantErr: "load config file",
		},
		{
			name: "spinifex start (start failure via blocked base-dir)",
			cmd:  spinifexStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeSpinifexToml(t, minimalClusterToml))
				viper.Set("base-dir", blockedDir(t))
			},
			wantErr: "start spinifex service",
		},
		{name: "awsgw start (missing config)", cmd: awsgwStartCmd},
		{
			name: "awsgw start (config file load failure)",
			cmd:  awsgwStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeMalformedToml(t))
			},
			wantErr: "load config file",
		},
		{
			name: "awsgw start (start failure via blocked base-dir)",
			cmd:  awsgwStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeSpinifexToml(t, minimalClusterToml))
				viper.Set("base-dir", blockedDir(t))
			},
			wantErr: "start awsgw service",
		},
		{
			name: "spinifex-ui start (start failure via missing tls cert)",
			cmd:  spinifexUIStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("spinifex-ui-base-dir", t.TempDir())
				viper.Set("spinifex-ui-tls-cert", filepath.Join(t.TempDir(), "missing.pem"))
			},
			wantErr: "start spinifex-ui service",
		},
		{name: "vpcd start (missing config)", cmd: vpcdStartCmd},
		{
			name: "vpcd start (config file load failure)",
			cmd:  vpcdStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeMalformedToml(t))
			},
			wantErr: "load config file",
		},
		{
			name: "vpcd start (legacy wan_bridge env rejected)",
			cmd:  vpcdStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeSpinifexToml(t, minimalClusterToml))
				t.Setenv("SPINIFEX_VPCD_WAN_BRIDGE", "br0")
			},
			wantErr: "deprecated 'wan_bridge'",
		},
		{
			name: "vpcd start (start failure via blocked base-dir)",
			cmd:  vpcdStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeSpinifexToml(t, minimalClusterToml))
				viper.Set("base-dir", blockedDir(t))
			},
			wantErr: "start vpcd service",
		},
		{name: "qmp-collector start (missing config)", cmd: qmpCollectorStartCmd},
		{
			name: "qmp-collector start (config file load failure)",
			cmd:  qmpCollectorStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("config", writeMalformedToml(t))
			},
			wantErr: "load config file",
		},
		{
			name: "qmp-collector start (start failure via blocked base-dir)",
			cmd:  qmpCollectorStartCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				toml := minimalClusterToml + fmt.Sprintf("base_dir = %q\n", blockedDir(t))
				viper.Set("config", writeSpinifexToml(t, toml))
			},
			wantErr: "start qmp-collector service",
		},
		// The stop commands below resolve their pid directory to an isolated,
		// always-empty temp dir, so Stop() safely fails to find a pid file
		// rather than risking a real running process via an ambient path.
		{
			name: "predastore stop (no running process)",
			cmd:  predastoreStopCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				viper.Set("predastore-base-path", t.TempDir())
			},
			wantErr: "stop predastore service",
		},
		{
			name: "viperblock stop (no running process)",
			cmd:  viperblockStopCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				isolateRuntimeDir(t)
			},
			wantErr: "stop viperblock service",
		},
		{
			name: "qemunbd stop (no running process)",
			cmd:  qemunbdStopCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				isolateRuntimeDir(t)
			},
			wantErr: "stop qemunbd service",
		},
		{
			name: "nats stop (no running process)",
			cmd:  natsStopCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				isolateRuntimeDir(t)
			},
			wantErr: "stop nats service",
		},
		{
			name: "spinifex stop (no running process)",
			cmd:  spinifexStopCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				isolateRuntimeDir(t)
			},
			wantErr: "stop spinifex service",
		},
		{
			name: "awsgw stop (no running process)",
			cmd:  awsgwStopCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				isolateRuntimeDir(t)
			},
			wantErr: "stop awsgw service",
		},
		{
			name: "spinifex-ui stop (no running process)",
			cmd:  spinifexUIStopCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				isolateRuntimeDir(t)
			},
			wantErr: "stop spinifex-ui service",
		},
		{
			name: "vpcd stop (no running process)",
			cmd:  vpcdStopCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				isolateRuntimeDir(t)
			},
			wantErr: "stop vpcd service",
		},
		{
			name: "northstar stop (no running process)",
			cmd:  northstarStopCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				isolateRuntimeDir(t)
			},
			wantErr: "stop northstar service",
		},
		{
			name: "qmp-collector stop (no running process)",
			cmd:  qmpCollectorStopCmd,
			setup: func(t *testing.T) {
				resetGlobalViper(t)
				isolateRuntimeDir(t)
			},
			wantErr: "stop qmp-collector service",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			require.NotNil(t, tt.cmd.RunE, "command must use RunE so a fatal error propagates to a non-zero exit")
			err := tt.cmd.RunE(tt.cmd, nil)
			require.Error(t, err, "expected a fatal startup error to be returned, not swallowed")
			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
			}
			assert.True(t, tt.cmd.SilenceErrors, "SilenceErrors must be set so cobra doesn't double-print the error root.go already reports")
			assert.True(t, tt.cmd.SilenceUsage, "SilenceUsage must be set so a startup failure doesn't dump command usage")
		})
	}
}

// TestSpinifexUIStatusCmdReportsStoppedWithNoRunningProcess drives the
// status command's success path: with no pid file at an isolated
// XDG_RUNTIME_DIR, Status reports "stopped" and RunE must return nil.
func TestSpinifexUIStatusCmdReportsStoppedWithNoRunningProcess(t *testing.T) {
	isolateRuntimeDir(t)

	require.NotNil(t, spinifexUIStatusCmd.RunE)
	err := spinifexUIStatusCmd.RunE(spinifexUIStatusCmd, nil)
	require.NoError(t, err)
}

// TestSpinifexUIStatusCmdReturnsErrorOnUnreadablePidDir drives the status
// command's other branch: a genuine read failure (permission denied, not
// ENOENT) must surface as an error rather than the benign "stopped" case.
func TestSpinifexUIStatusCmdReturnsErrorOnUnreadablePidDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "spinifex-ui.pid"), []byte("123"), 0o600))
	require.NoError(t, os.Chmod(dir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	t.Setenv("XDG_RUNTIME_DIR", dir)

	require.NotNil(t, spinifexUIStatusCmd.RunE)
	err := spinifexUIStatusCmd.RunE(spinifexUIStatusCmd, nil)
	require.ErrorContains(t, err, "get spinifex-ui service status")
}

// TestServiceStartExitsNonZeroOnFailure proves a fatal service-start error
// terminates the process non-zero (systemd's Restart=on-failure depends on
// it). os.Exit can't be observed in-process, so this re-execs the test
// binary as a subprocess and inspects its real exit code.
func TestServiceStartExitsNonZeroOnFailure(t *testing.T) {
	if os.Getenv("SPX_EXIT_CODE_TEST_HELPER") == "1" {
		// Subprocess mode: run a fatal startup path for real via Execute().
		// No flags/config supplied, so predastore start fails on host-id.
		rootCmd.SetArgs([]string{"service", "predastore", "start"})
		Execute()
		return
	}

	execCmd := exec.Command(os.Args[0], "-test.run=^TestServiceStartExitsNonZeroOnFailure$")
	execCmd.Env = append(os.Environ(), "SPX_EXIT_CODE_TEST_HELPER=1")
	out, err := execCmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected the subprocess to fail with a non-zero exit, got err=%v output=%s", err, out)
	}
	assert.Equal(t, 1, exitErr.ExitCode(), "a fatal service-start error must exit non-zero for systemd Restart=on-failure to fire; output=%s", out)
}
