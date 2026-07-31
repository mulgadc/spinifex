package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
)

func TestEngineProbe_MapsExitCodesToHealth(t *testing.T) {
	tests := []struct {
		name        string
		code        int
		runErr      error
		seenHealthy bool
		want        handlers_rds.EngineHealth
	}{
		{name: "accepting connections", code: 0, want: handlers_rds.EngineHealthHealthy},
		{name: "rejecting during startup", code: 1, want: handlers_rds.EngineHealthStarting},
		{name: "silent before first success", code: 2, want: handlers_rds.EngineHealthStarting},
		{name: "silent after first success", code: 2, seenHealthy: true, want: handlers_rds.EngineHealthUnhealthy},
		{name: "no attempt made", code: 3, want: handlers_rds.EngineHealthStarting},
		{name: "probe missing before first success", runErr: errors.New("executable not found"), want: handlers_rds.EngineHealthStarting},
		{name: "probe missing after first success", runErr: errors.New("executable not found"), seenHealthy: true, want: handlers_rds.EngineHealthUnhealthy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := newEngineProbe(testProbeConfig(), func(context.Context, string, ...string) (int, error) {
				return tt.code, tt.runErr
			})
			probe.seenHealthy = tt.seenHealthy

			got, message := probe.Check(context.Background())
			if got != tt.want {
				t.Errorf("health = %q, want %q", got, tt.want)
			}
			if got != handlers_rds.EngineHealthHealthy && message == "" {
				t.Error("a non-healthy result carried no message explaining it")
			}
			if got == handlers_rds.EngineHealthHealthy && message != "" {
				t.Errorf("a healthy result carried a message: %q", message)
			}
		})
	}
}

// The latch is what separates a boot still in progress from a failure, and it
// has to flip the first time the engine answers.
func TestEngineProbe_LatchesAfterFirstHealthy(t *testing.T) {
	code := 0
	probe := newEngineProbe(testProbeConfig(), func(context.Context, string, ...string) (int, error) {
		return code, nil
	})

	if got, _ := probe.Check(context.Background()); got != handlers_rds.EngineHealthHealthy {
		t.Fatalf("first check = %q, want healthy", got)
	}
	code = 2
	if got, _ := probe.Check(context.Background()); got != handlers_rds.EngineHealthUnhealthy {
		t.Errorf("check after the engine went away = %q, want unhealthy", got)
	}
}

// The port the engine listens on comes from the bootstrap config, so the probe
// must follow it rather than keep asking the default.
func TestEngineProbe_ProbesTheAssignedPort(t *testing.T) {
	var gotArgs []string
	probe := newEngineProbe(testProbeConfig(), func(_ context.Context, _ string, args ...string) (int, error) {
		gotArgs = args
		return 0, nil
	})

	probe.setPort(6543)
	probe.Check(context.Background())

	if !strings.Contains(strings.Join(gotArgs, " "), "-p 6543") {
		t.Errorf("probe args = %v, want the assigned port 6543", gotArgs)
	}
}

func testProbeConfig() config {
	return config{EngineHost: defaultEngineHost, EnginePort: defaultEnginePort, PGIsReady: defaultPGIsReady}
}
