package host

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFlushNeigh_ShellOutShape(t *testing.T) {
	r := &recordingRunner{}
	if err := FlushNeigh(context.Background(), r, "br-wan", "192.168.0.231"); err != nil {
		t.Fatalf("FlushNeigh: %v", err)
	}
	if len(r.args) != 1 {
		t.Fatalf("expected 1 call, got %d", len(r.args))
	}
	got := strings.Join(r.args[0], " ")
	want := "ip neigh flush to 192.168.0.231 dev br-wan"
	if got != want {
		t.Fatalf("argv mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestFlushNeigh_MissingArgs(t *testing.T) {
	if err := FlushNeigh(context.Background(), &recordingRunner{}, "", "192.168.0.231"); err == nil {
		t.Fatal("expected error for empty dev")
	}
	if err := FlushNeigh(context.Background(), &recordingRunner{}, "br-wan", ""); err == nil {
		t.Fatal("expected error for empty ip")
	}
}

func TestFlushNeigh_PropagatesRunnerError(t *testing.T) {
	r := &recordingRunner{out: []byte("Cannot find device \"br-wan\""), err: errors.New("exit 1")}
	if err := FlushNeigh(context.Background(), r, "br-wan", "192.168.0.231"); err == nil {
		t.Fatal("expected error")
	}
}

func TestReplaceNeigh_ShellOutShape(t *testing.T) {
	r := &recordingRunner{}
	if err := ReplaceNeigh(context.Background(), r, "br-wan", "192.168.0.231", "02:00:0a:d2:01:04"); err != nil {
		t.Fatalf("ReplaceNeigh: %v", err)
	}
	if len(r.args) != 1 {
		t.Fatalf("expected 1 call, got %d", len(r.args))
	}
	got := strings.Join(r.args[0], " ")
	want := "ip neigh replace 192.168.0.231 lladdr 02:00:0a:d2:01:04 dev br-wan nud reachable"
	if got != want {
		t.Fatalf("argv mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestReplaceNeigh_MissingArgs(t *testing.T) {
	if err := ReplaceNeigh(context.Background(), &recordingRunner{}, "", "192.168.0.231", "02:00:0a:d2:01:04"); err == nil {
		t.Fatal("expected error for empty dev")
	}
	if err := ReplaceNeigh(context.Background(), &recordingRunner{}, "br-wan", "", "02:00:0a:d2:01:04"); err == nil {
		t.Fatal("expected error for empty ip")
	}
	if err := ReplaceNeigh(context.Background(), &recordingRunner{}, "br-wan", "192.168.0.231", ""); err == nil {
		t.Fatal("expected error for empty mac")
	}
}

func TestReplaceNeigh_PropagatesRunnerError(t *testing.T) {
	r := &recordingRunner{out: []byte("Cannot find device \"br-wan\""), err: errors.New("exit 1")}
	if err := ReplaceNeigh(context.Background(), r, "br-wan", "192.168.0.231", "02:00:0a:d2:01:04"); err == nil {
		t.Fatal("expected error")
	}
}
