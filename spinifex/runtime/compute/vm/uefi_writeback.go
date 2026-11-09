package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	// uefiWriteBackQuiet is how long vars.fd must go unchanged before the
	// changes in an open window are written back.
	uefiWriteBackQuiet = 2 * time.Second
	// uefiWriteBackCap bounds how long a window stays open however often the
	// guest writes, so a continuous writer still lands every 30 s.
	uefiWriteBackCap = 30 * time.Second
	// uefiWriteBackRetry is how long a failed write-back waits to try again.
	uefiWriteBackRetry = 30 * time.Second
	// uefiRecordTimeout bounds one store record move a write-back makes.
	uefiRecordTimeout = 10 * time.Second
	// uefiWriteBackPoll is how often a writer without a working watcher
	// compares vars.fd with what last landed.
	uefiWriteBackPoll = 10 * time.Second
)

// uefiRegionSum is the hex SHA-256 of a VARS region.
func uefiRegionSum(region []byte) string {
	sum := sha256.Sum256(region)
	return hex.EncodeToString(sum[:])
}

// uefiCopyChanged reports whether dir's vars.fd may differ from the region
// that sums to sum. An unknown sum or an unreadable file counts as changed.
func uefiCopyChanged(dir, sum string) bool {
	if sum == "" {
		return true
	}
	region, err := os.ReadFile(filepath.Join(dir, uefiVarsFileName))
	return err != nil || uefiRegionSum(region) != sum
}

// UEFIClock is the clock the write-back's quiet period, cap and prune age run
// on. Nil in UEFIVarStoreDeps means the real clock.
type UEFIClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realUEFIClock struct{}

func (realUEFIClock) Now() time.Time                         { return time.Now() }
func (realUEFIClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (m *Manager) uefiClock() UEFIClock {
	if c := m.deps.UEFIVarStore.Clock; c != nil {
		return c
	}
	return realUEFIClock{}
}

// uefiWindow is the changes vars.fd has had since the last landed write-back:
// the first and the last, and the earliest a failed write-back may retry.
type uefiWindow struct {
	open        bool
	first, last time.Time
	notBefore   time.Time
}

// change records a change at now and reports whether it opened the window.
func (w *uefiWindow) change(now time.Time) bool {
	w.last = now
	if w.open {
		return false
	}
	w.open, w.first = true, now
	return true
}

// due is when the open window is written back: 2 s after its last change or
// 30 s after its first, whichever is sooner, and never before a retry may run.
func (w *uefiWindow) due() time.Time {
	d := w.last.Add(uefiWriteBackQuiet)
	if capped := w.first.Add(uefiWriteBackCap); capped.Before(d) {
		d = capped
	}
	if w.notBefore.After(d) {
		return w.notBefore
	}
	return d
}

func (w *uefiWindow) failed(now time.Time) { w.notBefore = now.Add(uefiWriteBackRetry) }
func (w *uefiWindow) landed()              { *w = uefiWindow{} }

// uefiWriteBackOutcome is what one write-back did.
type uefiWriteBackOutcome string

const (
	uefiWriteBackLanded uefiWriteBackOutcome = "landed"
	uefiWriteBackFailed uefiWriteBackOutcome = "failed"
	// uefiWriteBackFenced is a pointer move refused because a later claim
	// stamped the record: another node owns the store now.
	uefiWriteBackFenced uefiWriteBackOutcome = "fenced"
	// uefiWriteBackGone is a record deleting or absent: terminate owns it.
	uefiWriteBackGone uefiWriteBackOutcome = "deleting"
)

// uefiWriter writes one instance's vars.fd back to the object store under the
// claim token it holds. The goroutine's results are read after done closes.
type uefiWriter struct {
	id       string
	dir      string
	tok      kvsync.Token
	key      string
	fw       uefistore.FirmwareIdentity
	codePath string
	// sum is the SHA-256 of the region key holds, or empty when unknown.
	sum string

	cancel context.CancelFunc
	done   chan struct{}
	// dirty is set when the window was open as the writer stopped, or the
	// watcher was lost, so the changes may not have landed.
	dirty bool
	// stopped names why the writer stopped writing back: fenced or deleting.
	stopped uefiWriteBackOutcome
}

// uefiWriterState is the Manager's writers and the prefixes owed a prune.
type uefiWriterState struct {
	mu      sync.Mutex
	writers map[string]*uefiWriter
	prune   map[string]struct{}
	// locks serialises a launch's claim and registration with the stop's and
	// the reaper's write-back and unlock, per instance.
	locks keyedMutex
}

func (s *uefiWriterState) put(w *uefiWriter) *uefiWriter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writers == nil {
		s.writers = map[string]*uefiWriter{}
	}
	old := s.writers[w.id]
	s.writers[w.id] = w
	return old
}

func (s *uefiWriterState) get(id string) *uefiWriter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writers[id]
}

func (s *uefiWriterState) take(id string) *uefiWriter {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.writers[id]
	delete(s.writers, id)
	return w
}

func (s *uefiWriterState) queuePrune(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prune == nil {
		s.prune = map[string]struct{}{}
	}
	s.prune[id] = struct{}{}
}

func (s *uefiWriterState) drainPrune() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.prune))
	for id := range s.prune {
		ids = append(ids, id)
	}
	s.prune = nil
	return ids
}

// halt stops the writer's goroutine, if it started, and waits for it.
func (w *uefiWriter) halt() {
	if w.cancel == nil {
		return
	}
	w.cancel()
	<-w.done
}

// uefiWriteBackWired reports whether this node can write a store back.
func (m *Manager) uefiWriteBackWired() bool {
	deps := m.deps.UEFIVarStore
	return deps.Objects != nil && deps.Bucket != "" && deps.DataDir != "" && deps.Records != nil &&
		deps.Claims != nil && m.deps.NodeID != ""
}

// registerUEFIWriter records the writer for a store a launch has claimed and
// prepared, before the guest runs, so neither the reaper nor a stop settles
// the store under the launch. The caller holds the instance's lock.
func (m *Manager) registerUEFIWriter(id, dir string, tok kvsync.Token, key, sum string, fw NodeFirmware) {
	w := &uefiWriter{id: id, dir: dir, tok: tok, key: key, sum: sum, fw: fw.Identity, codePath: fw.Code}
	if old := m.uefi.put(w); old != nil {
		old.halt()
		slog.Warn("Replaced a UEFI variable store writer a new launch found registered", "instanceId", id)
	}
}

// startUEFIWriter watches the launch's vars.fd for writes. A store the launch
// reused may carry changes no object has, so its window opens at once. A
// watch that cannot be set up leaves the writer dirty, so the stop writes back.
func (m *Manager) startUEFIWriter(id string, open bool) {
	unlock := m.uefi.locks.lock(id)
	defer unlock()
	w := m.uefi.get(id)
	if w == nil || w.cancel != nil {
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err == nil {
		if err = watcher.Add(filepath.Join(w.dir, uefiVarsFileName)); err != nil {
			_ = watcher.Close()
		}
	}
	if err != nil {
		watcher = nil
		slog.Warn("Cannot watch the UEFI variable store; polling it for changes instead",
			"instanceId", id, "err", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel, w.done = cancel, make(chan struct{})
	go m.runUEFIWriter(ctx, w, watcher, open)
}

// runUEFIWriter writes vars.fd back once each window of changes is due. Every
// event and watcher error counts as a change, and none is counted, because
// inotify merges identical unread events.
func (m *Manager) runUEFIWriter(ctx context.Context, w *uefiWriter, watcher *fsnotify.Watcher, open bool) {
	defer close(w.done)
	var (
		events <-chan fsnotify.Event
		errs   <-chan error
	)
	if watcher != nil {
		defer func() { _ = watcher.Close() }()
		events, errs = watcher.Events, watcher.Errors
	}
	clock := m.uefiClock()
	var win uefiWindow
	change := func() {
		if win.change(clock.Now()) {
			m.writeUEFIOwed(w, true)
		}
	}
	if open {
		change()
	}
	for {
		var fire, poll <-chan time.Time
		if win.open {
			fire = clock.After(win.due().Sub(clock.Now()))
		}
		if events == nil {
			poll = clock.After(uefiWriteBackPoll)
		}
		select {
		case <-poll:
			if uefiCopyChanged(w.dir, w.sum) {
				change()
			}
		case <-ctx.Done():
			w.dirty = w.dirty || win.open
			return
		case _, ok := <-events:
			if !ok {
				events = nil
				w.dirty = true
			}
			change()
		case err, ok := <-errs:
			if !ok {
				errs = nil
				w.dirty = true
			} else {
				slog.Warn("UEFI variable store watcher reported an error; treating it as a change",
					"instanceId", w.id, "err", err)
			}
			change()
		case <-fire:
			if clock.Now().Before(win.due()) {
				continue
			}
			switch out := m.writeBackUEFI(ctx, w, "change"); out {
			case uefiWriteBackLanded:
				win.landed()
			case uefiWriteBackFailed:
				win.failed(clock.Now())
			default:
				w.stopped = out
				return
			}
		}
	}
}

// writeBackUEFI puts w's vars.fd at a fresh key and moves the store record's
// pointer to it under w's generation, then rewrites seeded.json. A start that
// adopts the claim reuses vars.fd whatever key seeded.json names, so nothing
// depends on that rewrite landing.
func (m *Manager) writeBackUEFI(ctx context.Context, w *uefiWriter, trigger string) uefiWriteBackOutcome {
	start := time.Now()
	out, key, sum, err := m.putAndMoveUEFI(ctx, w)
	recordUEFIWriteBack(out, trigger)
	attrs := []any{"instanceId", w.id, "generation", w.tok.Generation, "trigger", trigger,
		"writeback_ms", otelsetup.Millis(time.Since(start))}
	switch out {
	case uefiWriteBackLanded:
		w.key, w.sum = key, sum
		m.uefi.queuePrune(w.id)
		m.writeUEFIOwed(w, false)
		slog.Info("Wrote the UEFI variable store back", append(attrs, "key", key)...)
	case uefiWriteBackFenced:
		slog.Warn("A later claim fenced this node's UEFI variable store write-back; it stops writing back",
			append(attrs, "err", err)...)
	case uefiWriteBackGone:
		slog.Info("UEFI variable store is being deleted; not writing it back", append(attrs, "err", err)...)
	default:
		if key != "" {
			m.uefi.queuePrune(w.id)
		}
		slog.Warn("Failed to write the UEFI variable store back", append(attrs, "err", err)...)
	}
	return out
}

func (m *Manager) putAndMoveUEFI(ctx context.Context, w *uefiWriter) (uefiWriteBackOutcome, string, string, error) {
	deps := m.deps.UEFIVarStore
	region, err := os.ReadFile(filepath.Join(w.dir, uefiVarsFileName))
	if err != nil {
		return uefiWriteBackFailed, "", "", fmt.Errorf("read UEFI variable store: %w", err)
	}
	if int64(len(region)) != w.fw.VarsSize {
		return uefiWriteBackFailed, "", "", fmt.Errorf("vars.fd is %d bytes, and the store's VARS size is %d", len(region), w.fw.VarsSize)
	}
	key, err := uefistore.NewObjectKey(w.id)
	if err != nil {
		return uefiWriteBackFailed, "", "", err
	}
	if err := uefistore.PutObject(ctx, deps.Objects, deps.Bucket, key, region, uefistore.ObjectMeta{
		OwnerID: w.id, WriterNode: m.deps.NodeID, Firmware: w.fw, CodePath: w.codePath, Source: uefistore.SourceWriteBack,
	}); err != nil {
		return uefiWriteBackFailed, key, "", err
	}
	ref := uefistore.UEFIVarStoreRef{Key: key, Firmware: w.fw, WriterNode: m.deps.NodeID, WrittenAt: time.Now().UTC()}
	moveCtx, cancel := context.WithTimeout(ctx, uefiRecordTimeout)
	defer cancel()
	err = deps.Records.MovePointer(moveCtx, w.id, w.tok.Generation, ref)
	switch {
	case err == nil:
		return uefiWriteBackLanded, key, uefiRegionSum(region), nil
	case errors.Is(err, uefistore.ErrFenced):
		return uefiWriteBackFenced, key, "", err
	case errors.Is(err, uefistore.ErrDeleting), errors.Is(err, kvstore.ErrNotFound):
		return uefiWriteBackGone, key, "", err
	default:
		return uefiWriteBackFailed, key, "", err
	}
}

// writeUEFIOwed rewrites seeded.json for w with the owed mark set or cleared.
// A failed rewrite is logged: a set mark that does not land is covered by the
// next start reusing vars.fd, and a clear that does not land costs a write.
func (m *Manager) writeUEFIOwed(w *uefiWriter, owed bool) {
	if err := writeUEFISeed(w.dir, uefiSeed{
		Key: w.key, Generation: w.tok.Generation, SeededAt: time.Now().UTC(),
		Firmware: w.fw, CodePath: w.codePath, WriteBackOwed: owed, Sum: w.sum,
	}); err != nil {
		slog.Warn("Failed to rewrite the local UEFI variable store record", "instanceId", w.id, "owed", owed, "err", err)
	}
}

// uefiClaimState is who holds a store's claim against seeded.json.
type uefiClaimState int

const (
	// uefiClaimOurs is this node at seeded.json's generation.
	uefiClaimOurs uefiClaimState = iota
	// uefiClaimFree is no holder: the fence on the store record decides.
	uefiClaimFree
	// uefiClaimElsewhere is a later generation than seeded.json's, held by
	// any node: a lagging read can only show an older one.
	uefiClaimElsewhere
)

// uefiWriterFromSeed is the writer seeded.json describes and who holds its
// claim. err is set when either cannot be read.
func (m *Manager) uefiWriterFromSeed(ctx context.Context, id string) (*uefiWriter, uefiSeed, uefiClaimState, error) {
	dir, err := uefiInstanceDir(m.deps.UEFIVarStore.DataDir, id)
	if err != nil {
		return nil, uefiSeed{}, uefiClaimElsewhere, err
	}
	seed, err := readUEFISeed(dir)
	if err != nil {
		return nil, seed, uefiClaimElsewhere, err
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uefiClaimTimeout)
	defer cancel()
	holder, held, err := m.deps.UEFIVarStore.Claims.Get(id).Holder(readCtx)
	if err != nil {
		return nil, seed, uefiClaimElsewhere, fmt.Errorf("read UEFI store claim for %s: %w", id, err)
	}
	tok := kvsync.Token{Holder: m.deps.NodeID, Generation: seed.Generation}
	state := uefiClaimOurs
	switch {
	case !held:
		state = uefiClaimFree
	case holder == tok:
	case holder.Generation > tok.Generation:
		state = uefiClaimElsewhere
	default:
		// Generations only grow, so an older one is a lagging read, not an owner.
		return nil, seed, uefiClaimElsewhere, fmt.Errorf("UEFI store claim for %s reads as %s at generation %d, older than this node's %d: %w",
			id, holder.Holder, holder.Generation, tok.Generation, ErrMountRetryable)
	}
	return &uefiWriter{id: id, dir: dir, tok: tok, key: seed.Key, sum: seed.Sum, fw: seed.Firmware, codePath: seed.CodePath}, seed, state, nil
}

// settleUEFIStore writes w back when dirty and then unlocks its claim. A
// write-back that fails keeps the claim and marks seeded.json owed, which the
// reaper retries; one that is fenced or finds the store deleting unlocks
// nothing, because this node no longer owns the claim's store.
func (m *Manager) settleUEFIStore(ctx context.Context, w *uefiWriter, dirty bool, trigger, why string) uefiWriteBackOutcome {
	out := uefiWriteBackLanded
	if dirty {
		out = m.writeBackUEFI(ctx, w, trigger)
	}
	switch out {
	case uefiWriteBackLanded:
		m.unlockUEFIClaim(ctx, w.id, w.tok, why)
	case uefiWriteBackFailed:
		m.writeUEFIOwed(w, true)
	}
	return out
}

// finishUEFIWriteBack settles a stopped instance's store once QEMU has exited
// and before its volumes unmount: the last changes are written back and the
// claim unlocked, or the claim kept and the write-back owed. A store this
// launch did not register is settled from seeded.json when the claim is ours.
func (m *Manager) finishUEFIWriteBack(instance *VM) {
	if !usesUEFIVarStore(instance) || !m.uefiWriteBackWired() {
		return
	}
	unlock := m.uefi.locks.lock(instance.ID)
	defer unlock()
	ctx := context.Background()

	w := m.uefi.take(instance.ID)
	dirty := false
	if w != nil {
		w.halt()
		if w.stopped != "" {
			return
		}
		seed, err := readUEFISeed(w.dir)
		dirty = w.dirty || err != nil || seed.WriteBackOwed || uefiCopyChanged(w.dir, w.sum)
	} else {
		var (
			seed  uefiSeed
			claim uefiClaimState
			err   error
		)
		w, seed, claim, err = m.uefiWriterFromSeed(ctx, instance.ID)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && claim == uefiClaimElsewhere) {
			return
		}
		if err != nil {
			slog.Warn("Cannot settle the stopped instance's UEFI variable store; keeping its claim and copy",
				"instanceId", instance.ID, "err", err)
			return
		}
		dirty = seed.WriteBackOwed || uefiCopyChanged(w.dir, seed.Sum)
		if claim == uefiClaimFree && !dirty {
			return
		}
	}
	if out := m.settleUEFIStore(ctx, w, dirty, "stop", "the instance stopped with its variables written back"); out == uefiWriteBackFailed {
		slog.Warn("The stop could not write the UEFI variable store back; this node keeps its claim and retries",
			"instanceId", instance.ID, "generation", w.tok.Generation)
	}
}

// finishStoppedUEFIWriteBack settles a stopped instance's store once its QEMU
// is proven gone. Otherwise the writer stops and the write-back is owed, with
// the claim kept, so the write-back reaper settles it after QEMU exits.
func (m *Manager) finishStoppedUEFIWriteBack(instance *VM) {
	if !usesUEFIVarStore(instance) || !m.uefiWriteBackWired() || uefiQEMUGone(instance.ID) {
		m.finishUEFIWriteBack(instance)
		return
	}
	unlock := m.uefi.locks.lock(instance.ID)
	defer unlock()
	w := m.uefi.take(instance.ID)
	if w == nil {
		return
	}
	w.halt()
	if w.stopped != "" {
		return
	}
	m.writeUEFIOwed(w, true)
	slog.Warn("The stopped instance's QEMU is not proven gone; keeping the UEFI variable store's claim until it exits",
		"instanceId", instance.ID, "generation", w.tok.Generation)
}

// uefiQEMUGone reports whether an instance's QEMU is proven gone; replaced in tests.
var uefiQEMUGone = qemuProvenGone

// uefiFailedLaunchExitWait bounds how long a failed launch waits for the QEMU
// it killed to exit before settling the store it may have written.
const uefiFailedLaunchExitWait = 10 * time.Second

// settleFailedUEFILaunch settles the store of a launch that failed after QEMU
// started, as a stop does, once that QEMU is provably gone. A QEMU that may
// still run keeps its writer, so its changes go on landing.
func (m *Manager) settleFailedUEFILaunch(instance *VM) {
	if !usesUEFIVarStore(instance) || !m.uefiWriteBackWired() || m.uefi.get(instance.ID) == nil {
		return
	}
	pid := int(instance.qemuPID.Load())
	if pid > 0 {
		_ = hostprocess.WaitForProcessExit(pid, uefiFailedLaunchExitWait)
	}
	if !uefiQEMUExited(pid) {
		slog.Warn("A failed launch's QEMU may still be running; keeping its UEFI variable store writer",
			"instanceId", instance.ID, "pid", pid)
		return
	}
	m.finishUEFIWriteBack(instance)
}

// uefiQEMUExited reports whether pid is provably gone; replaced in tests.
var uefiQEMUExited = hostprocess.ProcessExited

// dropUEFIWriter stops an instance's writer without writing back, for a path
// that must not or need not, and counts the skip. A writer with nothing owed
// on a path whose instance moved unlocks the claim, so the new owner can start.
func (m *Manager) dropUEFIWriter(instanceID, path string, unlockClean bool) {
	if !m.uefiWriteBackWired() {
		return
	}
	unlock := m.uefi.locks.lock(instanceID)
	defer unlock()
	w := m.uefi.take(instanceID)
	if w == nil {
		return
	}
	w.halt()
	recordUEFIWriteBackSkip(path)
	slog.Info("Stopped writing the UEFI variable store back", "instanceId", instanceID, "path", path,
		"dirty", w.dirty, "stopped", string(w.stopped))
	if unlockClean && !w.dirty && w.stopped == "" && !uefiCopyChanged(w.dir, w.sum) {
		if seed, err := readUEFISeed(w.dir); err == nil && !seed.WriteBackOwed {
			m.unlockUEFIClaim(context.Background(), instanceID, w.tok, "the instance moved to another node")
		}
	}
}

// resumeUEFIWriter restarts the writer for a guest that outlived a daemon
// restart, with its window open, since the restart may have missed changes.
// One that cannot start now is retried by the write-back reaper.
func (m *Manager) resumeUEFIWriter(instance *VM) {
	m.startWriterFromSeed(instance, "reconnect")
}

// startWriterFromSeed registers and starts the writer seeded.json describes
// for a running guest, while this node holds the claim at its generation. The
// reaper's adoption logs quietly, since it retries every sweep.
func (m *Manager) startWriterFromSeed(instance *VM, trigger string) {
	if !usesUEFIVarStore(instance) || !m.uefiWriteBackWired() {
		return
	}
	reaper := trigger == "reaper"
	unlock := m.uefi.locks.lock(instance.ID)
	if reaper && (m.Status(instance) != StateRunning || m.uefi.get(instance.ID) != nil) {
		unlock()
		return
	}
	w, _, claim, err := m.uefiWriterFromSeed(context.Background(), instance.ID)
	held := err == nil && claim == uefiClaimOurs
	if held {
		if old := m.uefi.put(w); old != nil {
			old.halt()
		}
	}
	unlock()
	if !held {
		if reaper {
			slog.Debug("Not adopting the running instance's UEFI variable store yet",
				"instanceId", instance.ID, "claimHeldElsewhere", err == nil, "err", err)
			return
		}
		recordUEFIWriteBackSkip(trigger)
		slog.Warn("Cannot resume writing the UEFI variable store back after a restart; the write-back reaper retries",
			"instanceId", instance.ID, "claimHeldElsewhere", err == nil, "err", err)
		return
	}
	if reaper {
		slog.Info("Adopted a running instance's UEFI variable store writer", "instanceId", instance.ID)
	}
	m.startUEFIWriter(instance.ID, true)
}

var (
	uefiWriteBackMetricsOnce sync.Once
	uefiWriteBacks           metric.Int64Counter
	uefiWriteBackSkips       metric.Int64Counter
	uefiWriteBacksLost       metric.Int64Counter
	uefiPrunedObjects        metric.Int64Counter
)

func uefiWriteBackMetrics() {
	uefiWriteBackMetricsOnce.Do(func() {
		meter := otel.Meter(uefiStoreMeterName)
		var err error
		uefiWriteBacks, err = meter.Int64Counter("mulga.vm.uefi_store.write_backs",
			metric.WithDescription("UEFI variable store write-backs, by outcome (landed, failed, fenced, deleting) and trigger (change, stop, reaper)."),
			metric.WithUnit("{write}"))
		if err != nil {
			otel.Handle(err)
		}
		uefiWriteBackSkips, err = meter.Int64Counter("mulga.vm.uefi_store.write_back_skips",
			metric.WithDescription("UEFI variable store writers stopped without writing back, by the path that stopped them."),
			metric.WithUnit("{writer}"))
		if err != nil {
			otel.Handle(err)
		}
		uefiWriteBacksLost, err = meter.Int64Counter("mulga.vm.uefi_store.write_backs_lost",
			metric.WithDescription("Owed UEFI variable store write-backs dropped because another node took the claim over."),
			metric.WithUnit("{store}"))
		if err != nil {
			otel.Handle(err)
		}
		uefiPrunedObjects, err = meter.Int64Counter("mulga.vm.uefi_store.pruned_objects",
			metric.WithDescription("UEFI variable store objects a prune deleted because no pointer names them."),
			metric.WithUnit("{object}"))
		if err != nil {
			otel.Handle(err)
		}
	})
}

func recordUEFIWriteBack(out uefiWriteBackOutcome, trigger string) {
	uefiWriteBackMetrics()
	if uefiWriteBacks != nil {
		uefiWriteBacks.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("outcome", string(out)), attribute.String("trigger", trigger)))
	}
}

func recordUEFIWriteBackSkip(path string) {
	uefiWriteBackMetrics()
	if uefiWriteBackSkips != nil {
		uefiWriteBackSkips.Add(context.Background(), 1, metric.WithAttributes(attribute.String("path", path)))
	}
}

func recordUEFIWriteBackLost() {
	uefiWriteBackMetrics()
	if uefiWriteBacksLost != nil {
		uefiWriteBacksLost.Add(context.Background(), 1)
	}
}

func recordUEFIPruned(n int) {
	uefiWriteBackMetrics()
	if uefiPrunedObjects != nil && n > 0 {
		uefiPrunedObjects.Add(context.Background(), int64(n))
	}
}
