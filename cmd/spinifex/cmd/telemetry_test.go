package cmd

import "testing"

func TestInitTelemetryNoEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	tests := []struct {
		name  string
		debug bool
	}{
		{"info level", false},
		{"debug level", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flush := initTelemetry("test-svc", tt.debug)
			if flush == nil {
				t.Fatal("initTelemetry returned nil flush func")
			}
			flush()
		})
	}
}
