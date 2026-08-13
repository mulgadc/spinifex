package cmd

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServiceStartCmdsReturnErrorOnMissingConfig exercises each service
// start command's RunE directly (in-process, no subprocess needed) and
// asserts a fatal startup precondition failure surfaces as a returned
// error rather than a silent, zero-exit-code return. Cobra stores RunE on
// the Command value, so calling it directly is a normal Go function call.
func TestServiceStartCmdsReturnErrorOnMissingConfig(t *testing.T) {
	tests := []struct {
		name string
		cmd  *cobra.Command
	}{
		{name: "predastore start (missing host-id)", cmd: predastoreStartCmd},
		{name: "viperblock start (missing config)", cmd: viperblockStartCmd},
		{name: "qemunbd start (missing config)", cmd: qemunbdStartCmd},
		{name: "spinifex start (missing config)", cmd: spinifexStartCmd},
		{name: "awsgw start (missing config)", cmd: awsgwStartCmd},
		{name: "vpcd start (missing config)", cmd: vpcdStartCmd},
		{name: "qmp-collector start (missing config)", cmd: qmpCollectorStartCmd},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NotNil(t, tt.cmd.RunE, "command must use RunE so a fatal error propagates to a non-zero exit")
			err := tt.cmd.RunE(tt.cmd, nil)
			require.Error(t, err, "expected a fatal startup error to be returned, not swallowed")
			assert.True(t, tt.cmd.SilenceErrors, "SilenceErrors must be set so cobra doesn't double-print the error root.go already reports")
			assert.True(t, tt.cmd.SilenceUsage, "SilenceUsage must be set so a startup failure doesn't dump command usage")
		})
	}
}

// TestServiceStartExitsNonZeroOnFailure proves the end-to-end contract:
// a fatal service-start error must terminate the process with a non-zero
// exit code, the property systemd's Restart=on-failure depends on. os.Exit
// can't be observed in-process, so this re-execs the test binary as a
// subprocess (the standard Go idiom) and inspects its real exit code.
func TestServiceStartExitsNonZeroOnFailure(t *testing.T) {
	if os.Getenv("SPX_EXIT_CODE_TEST_HELPER") == "1" {
		// Subprocess mode: run a fatal startup path for real and let
		// Execute() reach os.Exit. No flags/config are supplied, so
		// predastore start fails fast on the missing host-id check.
		// SetArgs (not an os.Args reassignment) is cobra's own hook for this.
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
