package cmd_test

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/cmd/spinifex/cmd"
)

// handlerAtStartup samples the process logger once this test package's variables
// initialise — after every linked package's init() has run, before any test has.
// Sampling inside the test instead would read whatever the previously running
// test left behind, and several tests here start services that install their own
// logger legitimately.
var handlerAtStartup = fmt.Sprintf("%T", slog.Default().Handler())

// TestDefaultLoggerNotHijackedByLibrary guards the regression that made spx
// print JSON log records to stdout, interleaved with command output: a
// package-level init() in a linked library calling otelsetup.SetDefaultJSONLogger.
// Importing cmd pulls in the same package graph spx links, so if any of it
// installs a process logger again this fails.
func TestDefaultLoggerNotHijackedByLibrary(t *testing.T) {
	if strings.Contains(handlerAtStartup, "otelsetup") {
		t.Fatalf("a linked package installed %s as the process slog default; "+
			"setting the default belongs to the entrypoint, not a library init()", handlerAtStartup)
	}
}

func TestInitCLILoggerLevels(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		verbose bool
		want    slog.Level
	}{
		{name: "default is errors only", want: slog.LevelError},
		{name: "verbose raises to info", verbose: true, want: slog.LevelInfo},
		{name: "env sets level", env: "debug", want: slog.LevelDebug},
		{name: "verbose wins over env", env: "warn", verbose: true, want: slog.LevelInfo},
		{name: "invalid env is ignored", env: "chatty", want: slog.LevelError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SPX_LOG_LEVEL", tc.env)
			defer cmd.SetVerboseFlag(tc.verbose)()

			cmd.CLILogLevel.Set(slog.LevelError)
			cmd.InitCLILogger()

			if got := cmd.CLILogLevel.Level(); got != tc.want {
				t.Errorf("level = %v, want %v", got, tc.want)
			}
		})
	}
}
