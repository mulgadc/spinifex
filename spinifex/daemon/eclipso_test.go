package daemon

import (
	"context"
	"errors"
	"testing"
)

func TestEclipsoCtl_StartStopReload_InvokesSystemctl(t *testing.T) {
	original := runSystemctl
	t.Cleanup(func() { runSystemctl = original })

	var calls []string
	runSystemctl = func(_ context.Context, action, unit string) error {
		calls = append(calls, action+" "+unit)
		return nil
	}

	ctl := NewEclipsoCtl()

	if err := ctl.Stop(context.Background()); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if err := ctl.Reload(context.Background()); err != nil {
		t.Fatalf("Reload returned error: %v", err)
	}

	wantPrefix := []string{"stop " + EclipsoServiceName, "reload " + EclipsoServiceName}
	if len(calls) != len(wantPrefix) {
		t.Fatalf("calls = %v, want %d entries", calls, len(wantPrefix))
	}
	for i, want := range wantPrefix {
		if calls[i] != want {
			t.Errorf("calls[%d] = %q, want %q", i, calls[i], want)
		}
	}
}

func TestEclipsoCtl_Start_PropagatesError(t *testing.T) {
	original := runSystemctl
	t.Cleanup(func() { runSystemctl = original })

	want := errors.New("systemctl: boom")
	runSystemctl = func(_ context.Context, _, _ string) error { return want }

	ctl := NewEclipsoCtl()
	err := ctl.Start(context.Background())
	if err == nil {
		t.Fatal("Start returned nil error, want propagated systemctl failure")
	}
	if !errors.Is(err, want) {
		t.Errorf("Start error = %v, want it to wrap %v", err, want)
	}
}
