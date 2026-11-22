//go:build e2e

package storagefault

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// leaseGuardPoll, leaseGuardRead and leaseGuardValidity bound how long a
// replacement can outlive its lease. Two missed renewals is half viperblockd's
// 40 s validity, leaving room for a renewal that landed unacknowledged.
const (
	leaseGuardPoll     = 2 * time.Second
	leaseGuardRead     = 5 * time.Second
	leaseGuardValidity = 2 * 10 * time.Second
)

// replacementTermWait is how long a SIGTERM gets before SIGKILL.
const replacementTermWait = 10 * time.Second

// errReplacementUnknown means the node could not be asked, which is never
// evidence that the replacement exited.
var errReplacementUnknown = errors.New("replacement nbdkit state unknown")

// replacement is the hand-relaunched nbdkit, identified by pid and start time
// so a reused pid is never signalled or mistaken for it.
type replacement struct {
	f       *Fixture
	node    harness.Node
	socket  string
	pidFile string

	// launchMu makes a launch and a fence exclusive: a fence before the
	// launch cancels it, and one during it waits until the pid is known.
	launchMu sync.Mutex
	fenced   bool

	mu      sync.Mutex
	pid     int
	start   string
	spawned bool
	stopped chan struct{}
	cancel  context.CancelFunc
}

func newReplacement(f *Fixture, node harness.Node, socket, pidFile string) *replacement {
	return &replacement{f: f, node: node, socket: socket, pidFile: pidFile, cancel: func() {}}
}

// procStart returns a process's start time from /proc/<pid>/stat, or "" when
// the pid does not exist.
func (r *replacement) procStart(ctx context.Context, pid int) (string, error) {
	out, err := r.f.SSH.Run(ctx, r.node, fmt.Sprintf(
		"sudo sh -c 'if [ -e /proc/%d/stat ]; then awk \"{print \\$22}\" /proc/%d/stat; else echo GONE; fi'", pid, pid))
	if err != nil {
		return "", fmt.Errorf("%w: %v", errReplacementUnknown, err)
	}
	if s := strings.TrimSpace(string(out)); s != "GONE" {
		return s, nil
	}
	return "", nil
}

// lookup returns the pid the launch wrapper recorded, if that process is
// still nbdkit. It never reads argv: that read can block on a busy process.
func (r *replacement) lookup(ctx context.Context) ([]string, error) {
	out, err := r.f.SSH.Run(ctx, r.node, fmt.Sprintf(
		`sudo sh -c 'p=$(cat %s 2>/dev/null); case "$p" in ""|*[!0-9]*) exit 0;; esac; `+
			`c=$(cat /proc/$p/comm 2>/dev/null) || { [ -e /proc/$p ] && exit 4; exit 0; }; `+
			`[ "$c" = %s ] && echo $p; exit 0'`,
		r.pidFile, nbdkitComm))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errReplacementUnknown, err)
	}
	return strings.Fields(string(out)), nil
}

// resolve records the replacement's pid and start time, reporting false when
// no recorded nbdkit is running.
func (r *replacement) resolve(ctx context.Context) (bool, error) {
	pids, err := r.lookup(ctx)
	if err != nil {
		return false, err
	}
	switch len(pids) {
	case 0:
		return false, nil
	case 1:
	default:
		return false, fmt.Errorf("%w: %d pids in %s", errReplacementUnknown, len(pids), r.pidFile)
	}
	pid, err := strconv.Atoi(pids[0])
	if err != nil {
		return false, fmt.Errorf("%w: pid %q: %v", errReplacementUnknown, pids[0], err)
	}
	start, err := r.procStart(ctx, pid)
	if err != nil || start == "" {
		return false, err
	}
	r.mu.Lock()
	r.pid, r.start = pid, start
	r.mu.Unlock()
	return true, nil
}

// gone reports whether the replacement has exited; an unanswered query is an
// error, never a yes.
func (r *replacement) gone(ctx context.Context) (bool, error) {
	r.mu.Lock()
	pid, start, spawned := r.pid, r.start, r.spawned
	r.mu.Unlock()
	if !spawned {
		return true, nil
	}
	if pid == 0 {
		found, err := r.resolve(ctx)
		if err != nil {
			return false, err
		}
		if !found {
			return true, nil
		}
		r.mu.Lock()
		pid, start = r.pid, r.start
		r.mu.Unlock()
	}
	now, err := r.procStart(ctx, pid)
	if err != nil {
		return false, err
	}
	return now != start, nil
}

// signal sends sig to the replacement only if its pid still has the recorded
// start time.
func (r *replacement) signal(ctx context.Context, sig string) {
	r.mu.Lock()
	pid, start := r.pid, r.start
	r.mu.Unlock()
	if pid == 0 {
		if found, err := r.resolve(ctx); !found || err != nil {
			return
		}
		r.mu.Lock()
		pid, start = r.pid, r.start
		r.mu.Unlock()
	}
	_, _ = r.f.SSH.Run(ctx, r.node, fmt.Sprintf(
		`[ "$(sudo awk '{print $22}' /proc/%d/stat 2>/dev/null)" = %q ] && sudo kill -%s %d; true`, pid, start, sig, pid))
}

// awaitGone polls until the replacement has exited or wait passes.
func (r *replacement) awaitGone(ctx context.Context, wait time.Duration) (bool, error) {
	deadline := time.Now().Add(wait)
	var last error
	for {
		ok, err := r.gone(ctx)
		if ok {
			return true, nil
		}
		last = err
		if time.Now().After(deadline) {
			return false, last
		}
		time.Sleep(time.Second)
	}
}

// fence SIGKILLs the replacement at once and confirms it exited. Taking
// launchMu first means a pending launch either never starts or has finished
// and recorded its pid.
func (r *replacement) fence(ctx context.Context) (bool, error) {
	r.launchMu.Lock()
	r.fenced = true
	r.launchMu.Unlock()
	r.signal(ctx, "KILL")
	return r.awaitGone(ctx, 30*time.Second)
}

// launch runs start unless a fence came first, then records the pid while
// still holding launchMu, so the process is identified as it is launched.
func (r *replacement) launch(ctx context.Context, start func() error) error {
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	if r.fenced {
		return errors.New("the lease guard fenced the replacement before it launched")
	}
	r.mu.Lock()
	r.spawned = true
	r.mu.Unlock()
	if err := start(); err != nil {
		return err
	}
	if _, err := r.resolve(ctx); err != nil {
		return fmt.Errorf("identify the replacement at launch: %w", err)
	}
	return nil
}

// isFenced reports whether the guard has fenced the replacement.
func (r *replacement) isFenced() bool {
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	return r.fenced
}

// reap stops the replacement, SIGTERM then SIGKILL, and reports whether its
// exit was confirmed. Precondition: the guest has stopped using the disk.
func (r *replacement) reap(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r.signal(ctx, "TERM")
	ok, err := r.awaitGone(ctx, replacementTermWait)
	if !ok {
		ok, err = r.fence(ctx)
	}
	if !ok {
		t.Errorf("replacement nbdkit on %s did not exit: %v", r.socket, err)
		return false
	}
	r.mu.Lock()
	pid := r.pid
	r.mu.Unlock()
	t.Logf("replacement nbdkit pid %d on %s confirmed exited", pid, r.node.Name)
	if _, err := r.f.SSH.Run(ctx, r.node, fmt.Sprintf("sudo rm -f %s", r.socket)); err != nil {
		t.Logf("could not remove %s on %s: %v", r.socket, r.node.Name, err)
	}
	return true
}

// guard fences the replacement as soon as the lease it was started under is
// missing, changes hands or goes leaseGuardValidity without a renewal. It is
// armed before the spawn, seeded with the renewal that authorised the launch.
func (r *replacement) guard(t *testing.T, volumeID string, atKill leaseSnapshot, seenRev uint64, seenAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	kv := leaseKV(ctx, t, r.f)
	r.stopped = make(chan struct{})
	r.cancel = cancel
	go func() {
		defer close(r.stopped)
		lastRev, renewedAt := seenRev, seenAt
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(leaseGuardPoll):
			}
			why := r.check(ctx, kv, volumeID, atKill, &lastRev, &renewedAt)
			if ctx.Err() != nil || why == "" {
				continue
			}
			ok, err := r.fence(context.Background())
			t.Errorf("fenced the replacement nbdkit for %s: %s (exited=%v, err=%v)", volumeID, why, ok, err)
			return
		}
	}()
}

// check is one guard poll; it returns why the replacement must stop, or "".
func (r *replacement) check(ctx context.Context, kv jetstream.KeyValue, volumeID string, atKill leaseSnapshot, lastRev *uint64, renewedAt *time.Time) string {
	readCtx, cancel := context.WithTimeout(ctx, leaseGuardRead)
	defer cancel()
	snap, err := readLease(readCtx, kv, volumeID)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted):
		return "lease entry is gone"
	case err == nil && (snap.Owner != atKill.Owner || snap.Generation != atKill.Generation):
		return fmt.Sprintf("lease is now %s generation %d", snap.Owner, snap.Generation)
	case err == nil && snap.revision != *lastRev:
		*lastRev, *renewedAt = snap.revision, time.Now()
	}
	if since := time.Since(*renewedAt); since > leaseGuardValidity {
		return fmt.Sprintf("no renewal seen for %s (last read error: %v)", since.Round(time.Second), err)
	}
	return ""
}

// stopGuard ends the guard and waits for it.
func (r *replacement) stopGuard() {
	r.cancel()
	if r.stopped != nil {
		<-r.stopped
	}
}
