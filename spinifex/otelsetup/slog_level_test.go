package otelsetup

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

// recordingHandler stands in for the OTLP bridge: it only needs to prove it still
// receives records after a level change.
type recordingHandler struct{ msgs *[]string }

func (recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h recordingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.msgs = append(*h.msgs, r.Message)
	return nil
}
func (h recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h recordingHandler) WithGroup(string) slog.Handler      { return h }

// A gateway that reinstalled the default logger to change verbosity silently detached
// the OTLP bridge, so only startup lines ever reached the sink.
func TestSetLevelKeepsFanoutAttached(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var got []string
	SetDefaultJSONLogger(slog.LevelInfo)
	addFanoutHandler(recordingHandler{msgs: &got})

	SetLevel(slog.LevelDebug)
	slog.Info("after level change")

	if len(got) != 1 || got[0] != "after level change" {
		t.Fatalf("fanout handler lost after SetLevel, got %v", got)
	}

	// The hazard SetLevel exists to avoid: reinstalling the default drops the fanout,
	// so nothing logged afterwards reaches the bridge.
	SetDefaultJSONLogger(slog.LevelInfo)
	slog.Info("after reinstall")
	if len(got) != 1 {
		t.Fatalf("expected reinstall to detach the fanout, got %v", got)
	}
}

func TestSetLevelChangesVerbosityInPlace(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: defaultLevel})))

	SetLevel(slog.LevelError)
	slog.Info("suppressed")
	if buf.Len() != 0 {
		t.Fatalf("info logged at error level: %s", buf.String())
	}

	SetLevel(slog.LevelDebug)
	slog.Debug("emitted")
	if !bytes.Contains(buf.Bytes(), []byte("emitted")) {
		t.Fatalf("debug not logged after level lowered: %s", buf.String())
	}
}
