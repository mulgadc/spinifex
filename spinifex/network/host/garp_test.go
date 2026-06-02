package host

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recordingRunner struct {
	args [][]string
	out  []byte
	err  error
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	full := append([]string{name}, args...)
	r.args = append(r.args, full)
	return r.out, r.err
}

func TestInjectGARP_ShellOutShape(t *testing.T) {
	r := &recordingRunner{}
	if err := InjectGARP(context.Background(), r, "port-eni-abc"); err != nil {
		t.Fatalf("InjectGARP: %v", err)
	}
	if len(r.args) != 1 {
		t.Fatalf("expected 1 call, got %d", len(r.args))
	}
	got := strings.Join(r.args[0], " ")
	want := "ovn-appctl -t ovn-controller inject-garp port-eni-abc"
	if got != want {
		t.Fatalf("argv mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestInjectGARP_CentralizedPort(t *testing.T) {
	r := &recordingRunner{}
	if err := InjectGARP(context.Background(), r, "cr-gw-vpc-abc"); err != nil {
		t.Fatalf("InjectGARP: %v", err)
	}
	got := strings.Join(r.args[0], " ")
	want := "ovn-appctl -t ovn-controller inject-garp cr-gw-vpc-abc"
	if got != want {
		t.Fatalf("argv mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestInjectGARP_MissingPort(t *testing.T) {
	if err := InjectGARP(context.Background(), &recordingRunner{}, ""); err == nil {
		t.Fatal("expected error for empty logicalPort")
	}
}

func TestInjectGARP_PropagatesRunnerError(t *testing.T) {
	r := &recordingRunner{out: []byte("ovn-controller: no such port"), err: errors.New("exit 2")}
	err := InjectGARP(context.Background(), r, "port-missing")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no such port") {
		t.Fatalf("expected combined output in error, got %v", err)
	}
}
