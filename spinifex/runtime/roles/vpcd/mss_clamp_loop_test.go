//test:in-package — runMSSClampLoop's interval is an unexported var, and the
// test has to shrink it to run at all.

package vpcd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
)

// kernelRunner models the kernel's view of our nft table: a batch that
// defines the clamp chain installs it, and the test can flush it.
type kernelRunner struct {
	mu        sync.Mutex
	installed bool
	batches   int
	err       error
	pass      chan struct{}
}

var _ host.Runner = (*kernelRunner)(nil)

func (k *kernelRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.batches++
	if k.err == nil && strings.Contains(args[0], "chain clamp") {
		k.installed = true
	}
	select {
	case k.pass <- struct{}{}:
	default:
	}
	return nil, k.err
}

func (k *kernelRunner) state() (installed bool, batches int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.installed, k.batches
}

func (k *kernelRunner) flush() {
	k.mu.Lock()
	k.installed = false
	k.mu.Unlock()
}

func withShortMSSClampInterval(t *testing.T) {
	t.Helper()
	old := mssClampInterval
	mssClampInterval = 10 * time.Millisecond
	t.Cleanup(func() { mssClampInterval = old })
}

func waitPass(t *testing.T, k *kernelRunner) {
	t.Helper()
	select {
	case <-k.pass:
	case <-time.After(2 * time.Second):
		t.Fatal("no clamp pass within 2s")
	}
}

// A host firewall reload flushes the table; the next pass must put it back.
func TestRunMSSClampLoopReinstallsAFlushedTable(t *testing.T) {
	withShortMSSClampInterval(t)
	k := &kernelRunner{pass: make(chan struct{}, 1)}
	go runMSSClampLoop(t.Context(), k, host.UplinkModeVeth, 1408)

	waitPass(t, k)
	if installed, _ := k.state(); !installed {
		t.Fatal("clamp not installed by the first pass")
	}
	k.flush()
	waitPass(t, k)
	for range 50 {
		if installed, _ := k.state(); installed {
			return
		}
		waitPass(t, k)
	}
	t.Error("clamp not reinstalled after the table was flushed")
}

func TestRunMSSClampLoopSurvivesAFailedPass(t *testing.T) {
	withShortMSSClampInterval(t)
	k := &kernelRunner{pass: make(chan struct{}, 1), err: errors.New("nft: operation not permitted")}
	go runMSSClampLoop(t.Context(), k, host.UplinkModeRouted, 1408)

	waitPass(t, k)
	waitPass(t, k)
	if _, batches := k.state(); batches < 2 {
		t.Errorf("loop stopped after a failed pass: %d batches", batches)
	}
}

func TestRunMSSClampLoopStopsOnCancel(t *testing.T) {
	withShortMSSClampInterval(t)
	k := &kernelRunner{pass: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { runMSSClampLoop(ctx, k, host.UplinkModeVeth, 1408); close(done) }()
	waitPass(t, k)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop after cancel")
	}
}
