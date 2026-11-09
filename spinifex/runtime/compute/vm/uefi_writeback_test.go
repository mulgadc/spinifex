package vm

import (
	"bytes"
	"context"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeUEFIClock is a clock the test advances. armed is signalled each time a
// writer waits on it, so a test knows the writer has seen a change.
type fakeUEFIClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeUEFIWaiter
	armed   chan struct{}
}

type fakeUEFIWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeUEFIClock(now time.Time) *fakeUEFIClock {
	return &fakeUEFIClock{now: now, armed: make(chan struct{}, 1024)}
}

func (c *fakeUEFIClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeUEFIClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
	} else {
		c.waiters = append(c.waiters, fakeUEFIWaiter{at: c.now.Add(d), ch: ch})
	}
	select {
	case c.armed <- struct{}{}:
	default:
	}
	return ch
}

func (c *fakeUEFIClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
}

// waitArmed waits for a writer to start waiting on the clock.
func (c *fakeUEFIClock) waitArmed(t *testing.T) {
	t.Helper()
	select {
	case <-c.armed:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer never saw the change")
	}
}

// running is a new instance seeded and prepared on node-a, with its writer
// registered and the fixture's clock set, as a launch leaves it before QEMU.
func (f *uefiFixture) running(t *testing.T) (*VM, *fakeUEFIClock) {
	t.Helper()
	clock := newFakeUEFIClock(time.Now())
	f.m.deps.UEFIVarStore.Clock = clock
	instance := f.newInstance()
	require.NoError(t, f.m.SeedUEFIVarStore(t.Context(), instance))
	held, err := f.prepare(t, instance)
	require.NoError(t, err)
	require.True(t, held.reused)
	instance.Status = StateRunning
	f.m.Insert(instance)
	t.Cleanup(func() {
		if w := f.m.uefi.take(testUEFIInstance); w != nil {
			w.halt()
		}
	})
	return instance, clock
}

// guestWrites writes tag at the start of vars.fd in place, as QEMU's pwrite
// does, and returns the region the file now holds.
func (f *uefiFixture) guestWrites(t *testing.T, tag string) []byte {
	t.Helper()
	fh, err := os.OpenFile(filepath.Join(f.instanceDir(), "vars.fd"), os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = fh.WriteAt([]byte(tag), 0)
	require.NoError(t, err)
	require.NoError(t, fh.Close())
	return region(tag)
}

// pointedRegion is the decompressed object the store record points at.
func (f *uefiFixture) pointedRegion(t *testing.T) []byte {
	t.Helper()
	rec, _ := f.record(t)
	require.NotNil(t, rec.Pointer)
	return gunzip(t, f.objectBody(t, rec.Pointer.Key))
}

// failingPutStore refuses every put, as an unreachable predastore does.
type failingPutStore struct {
	*objectstore.MemoryObjectStore
}

func (s failingPutStore) PutObject(context.Context, *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	return nil, assert.AnError
}

func TestUEFIWindow_IsDueAfterTheQuietPeriodOrTheCap(t *testing.T) {
	t0 := time.Unix(1700000000, 0)

	var quiet uefiWindow
	assert.True(t, quiet.change(t0), "the first change opens the window")
	assert.False(t, quiet.change(t0.Add(time.Second)))
	assert.Equal(t, t0.Add(3*time.Second), quiet.due(), "2 s after the last change")

	var busy uefiWindow
	for i := range 40 {
		busy.change(t0.Add(time.Duration(i) * time.Second))
	}
	assert.Equal(t, t0.Add(30*time.Second), busy.due(), "a guest writing every second still lands 30 s after its first change")

	busy.failed(t0.Add(40 * time.Second))
	assert.Equal(t, t0.Add(70*time.Second), busy.due(), "a failed write-back waits 30 s to retry")

	busy.landed()
	assert.False(t, busy.open)
	assert.True(t, busy.change(t0.Add(80*time.Second)), "a change after a landed write-back opens the next window")
}

// A write the guest makes is marked owed at once and lands once the quiet
// period passes: a fresh object, the pointer moved, seeded.json rewritten.
func TestUEFIWriter_WritesAChangeBackAfterTheQuietPeriod(t *testing.T) {
	f := newUEFIFixture(t)
	_, clock := f.running(t)
	before, _ := f.record(t)
	landed := uefiCounter(t, "mulga.vm.uefi_store.write_backs", "outcome", "landed")

	f.m.startUEFIWriter(testUEFIInstance, false)
	want := f.guestWrites(t, "boot-order")
	clock.waitArmed(t)
	assert.True(t, f.seed(t).WriteBackOwed, "an open window is owed before it lands")
	assert.Equal(t, before.Pointer.Key, f.mustRecord(t).Pointer.Key, "nothing lands inside the quiet period")

	require.Eventually(t, func() bool {
		clock.Advance(uefiWriteBackQuiet)
		return f.mustRecord(t).Pointer.Key != before.Pointer.Key
	}, 10*time.Second, 10*time.Millisecond)

	assert.Equal(t, want, f.pointedRegion(t))
	rec := f.mustRecord(t)
	assert.Equal(t, before.Generation, rec.Generation)
	assert.Len(t, f.objectKeys(t), 2, "the template stays until a prune")
	require.Eventually(t, func() bool {
		seed := f.seed(t)
		return seed.Key == rec.Pointer.Key && !seed.WriteBackOwed
	}, 10*time.Second, 10*time.Millisecond)
	assert.Equal(t, landed+1, uefiCounter(t, "mulga.vm.uefi_store.write_backs", "outcome", "landed"))
	assert.Contains(t, f.m.uefi.drainPrune(), testUEFIInstance)
}

func (f *uefiFixture) mustRecord(t *testing.T) *uefistore.UEFIStoreRecord {
	t.Helper()
	rec, _ := f.record(t)
	return rec
}

// A stop writes the guest's last changes back after QEMU exits and then
// unlocks the claim, so another node can start the instance from them.
func TestFinishUEFIWriteBack_WritesTheLastChangesBackAndUnlocks(t *testing.T) {
	f := newUEFIFixture(t)
	instance, clock := f.running(t)
	f.m.startUEFIWriter(testUEFIInstance, false)
	want := f.guestWrites(t, "shutdown")
	clock.waitArmed(t)

	f.m.finishUEFIWriteBack(instance)

	assert.Equal(t, want, f.pointedRegion(t))
	_, held := f.claimHolder(t)
	assert.False(t, held)
	assert.False(t, f.seed(t).WriteBackOwed)
	assert.Nil(t, f.m.uefi.get(testUEFIInstance))
}

// A guest that changed nothing since the last landed write-back stops
// without a put, and its claim is unlocked.
func TestFinishUEFIWriteBack_UnchangedStoreUnlocksWithoutAWrite(t *testing.T) {
	f := newUEFIFixture(t)
	instance, _ := f.running(t)
	f.m.startUEFIWriter(testUEFIInstance, false)

	f.m.finishUEFIWriteBack(instance)

	assert.Len(t, f.objectKeys(t), 1)
	_, held := f.claimHolder(t)
	assert.False(t, held)
}

// A stop whose write-back fails completes, keeps the claim and the copy and
// marks the write-back owed; the reaper lands it once the store answers and
// unlocks.
func TestFinishUEFIWriteBack_FailedWriteIsOwedAndTheReaperLandsIt(t *testing.T) {
	f := newUEFIFixture(t)
	instance, clock := f.running(t)
	tok, _ := f.claimHolder(t)
	f.m.startUEFIWriter(testUEFIInstance, false)
	want := f.guestWrites(t, "unlanded")
	clock.waitArmed(t)
	f.m.deps.UEFIVarStore.Objects = failingPutStore{f.objects}

	f.m.finishUEFIWriteBack(instance)

	holder, held := f.claimHolder(t)
	require.True(t, held, "a start elsewhere stays refused while this node owes the write-back")
	assert.Equal(t, tok, holder)
	assert.True(t, f.seed(t).WriteBackOwed)
	assert.Equal(t, want, f.localVars(t))

	require.True(t, f.m.UpdateState(testUEFIInstance, func(v *VM) { v.Status = StateStopped }))
	reaper := f.m.NewUEFIWriteBackReaper()
	require.NotNil(t, reaper)
	n, err := reaper.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n, "the store still refuses the put")
	_, held = f.claimHolder(t)
	assert.True(t, held)

	f.m.deps.UEFIVarStore.Objects = f.objects
	n, err = reaper.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, want, f.pointedRegion(t))
	_, held = f.claimHolder(t)
	assert.False(t, held)
	assert.False(t, f.seed(t).WriteBackOwed)
}

// After another node takes the claim over and stamps the record, this node's
// pointer move is fenced: it is counted, nothing moves, and the stop leaves
// the new holder's claim alone.
func TestUEFIWriter_FencedMoveStopsWritingBackAndIsCounted(t *testing.T) {
	f := newUEFIFixture(t)
	instance, clock := f.running(t)
	old, _ := f.claimHolder(t)
	tok, err := f.peerClaim("node-b").TakeOver(t.Context(), old)
	require.NoError(t, err)
	_, err = f.records.Stamp(t.Context(), testUEFIInstance, tok.Generation)
	require.NoError(t, err)
	before := f.mustRecord(t).Pointer.Key
	fenced := uefiCounter(t, "mulga.vm.uefi_store.write_backs", "outcome", "fenced")

	f.m.startUEFIWriter(testUEFIInstance, false)
	f.guestWrites(t, "stale")
	clock.waitArmed(t)
	require.Eventually(t, func() bool {
		clock.Advance(uefiWriteBackQuiet)
		return uefiCounter(t, "mulga.vm.uefi_store.write_backs", "outcome", "fenced") == fenced+1
	}, 10*time.Second, 10*time.Millisecond)
	f.m.finishUEFIWriteBack(instance)

	assert.Equal(t, before, f.mustRecord(t).Pointer.Key)
	holder, held := f.claimHolder(t)
	require.True(t, held)
	assert.Equal(t, tok, holder)
}

// pflash unit 1 never takes aio=native, because inotify does not see Linux AIO
// writes and the write-back would never fire.
func TestUEFIVarsDrive_NeverTakesAIONative(t *testing.T) {
	f := newUEFIFixture(t)
	f.writeLocal(t, keyFor("boot"), 1, region("seeded"))
	instance := f.instance(t, keyFor("boot"), f.identity)

	drive, err := f.m.uefiVarsDrive(instance)
	require.NoError(t, err)
	withQEMUResolver(t, func(string) (string, error) { return "qemu-system-x86_64", nil })
	cfg := minimalConfig()
	cfg.Drives = []Drive{*drive}
	cmd, err := cfg.Execute()
	require.NoError(t, err)

	drives := allArgValues(cmd.Args, "-drive")
	require.Len(t, drives, 1)
	assert.Contains(t, drives[0], "if=pflash,unit=1")
	assert.NotContains(t, drives[0], "aio=")
}

// A guest that outlived a daemon restart resumes write-back with its window
// open, so a change the restart missed still lands.
func TestResumeUEFIWriter_WritesBackWhatARestartMayHaveMissed(t *testing.T) {
	f := newUEFIFixture(t)
	instance, clock := f.running(t)
	f.m.uefi.take(testUEFIInstance)
	want := f.guestWrites(t, "missed")

	f.m.resumeUEFIWriter(instance)
	clock.waitArmed(t)
	require.Eventually(t, func() bool {
		clock.Advance(uefiWriteBackQuiet)
		return string(f.pointedRegion(t)[:6]) == "missed"
	}, 10*time.Second, 10*time.Millisecond)
	assert.Equal(t, want, f.pointedRegion(t))
}

// A writer that cannot watch vars.fd polls it against what last landed, so a
// change still lands while the guest runs rather than only at the stop.
func TestUEFIWriter_PollsWhenItCannotWatch(t *testing.T) {
	f := newUEFIFixture(t)
	_, clock := f.running(t)
	w := f.m.uefi.get(testUEFIInstance)
	ctx, cancel := context.WithCancel(t.Context())
	w.cancel, w.done = cancel, make(chan struct{})
	go f.m.runUEFIWriter(ctx, w, nil, false)
	clock.waitArmed(t)
	want := f.guestWrites(t, "polled")

	require.Eventually(t, func() bool {
		clock.Advance(uefiWriteBackPoll)
		return bytes.Equal(want, f.pointedRegion(t))
	}, 10*time.Second, 10*time.Millisecond)
}

// flakyClaimStore fails as many claim reads and deletes as the test arms, as
// a NATS election does, then passes through.
type flakyClaimStore struct {
	kvsync.KVStore

	failLoads, failDeletes, loads atomic.Int32
}

func (s *flakyClaimStore) Load(ctx context.Context, key string) ([]byte, uint64, error) {
	s.loads.Add(1)
	if s.failLoads.Add(-1) >= 0 {
		return nil, 0, kvsync.ErrUnavailable
	}
	return s.KVStore.Load(ctx, key)
}

func (s *flakyClaimStore) CompareAndDelete(ctx context.Context, key string, rev uint64) error {
	if s.failDeletes.Add(-1) >= 0 {
		return kvsync.ErrUnavailable
	}
	return s.KVStore.CompareAndDelete(ctx, key, rev)
}

func (f *uefiFixture) flakyClaims() *flakyClaimStore {
	store := &flakyClaimStore{KVStore: kvstore.OpenSyncBucket(f.js)}
	f.m.deps.UEFIVarStore.Claims = uefistore.NewClaims(store, "node-a")
	return store
}

// A clean copy's claim that the stop failed to unlock, or that no writer was
// left to unlock after a restart, is released by the reaper, so another node
// can start the instance. The copy itself stays.
func TestUEFIWriteBackReaper_UnlocksTheClaimOverACleanCopy(t *testing.T) {
	t.Run("unlock failed at stop", func(t *testing.T) {
		f := newUEFIFixture(t)
		flaky := f.flakyClaims()
		instance, _ := f.running(t)
		flaky.failDeletes.Store(1)
		f.m.finishUEFIWriteBack(instance)
		_, held := f.claimHolder(t)
		require.True(t, held, "the stop's unlock failed")
		require.True(t, f.m.UpdateState(testUEFIInstance, func(v *VM) { v.Status = StateStopped }))
		pointer, objects := f.mustRecord(t).Pointer.Key, f.objectKeys(t)

		_, err := f.m.NewUEFIWriteBackReaper().Sweep(t.Context())
		require.NoError(t, err)

		_, held = f.claimHolder(t)
		assert.False(t, held)
		assert.Equal(t, pointer, f.mustRecord(t).Pointer.Key, "a clean copy is not written back")
		assert.Equal(t, objects, f.objectKeys(t))
		assert.Equal(t, make([]byte, testUEFIVarsSize), f.localVars(t))
	})
	t.Run("no writer after a restart", func(t *testing.T) {
		f := newUEFIFixture(t)
		f.running(t)
		f.m.uefi.take(testUEFIInstance).halt()
		f.m.dropUEFIWriter(testUEFIInstance, "superseded", true)
		f.m.Delete(testUEFIInstance)
		_, held := f.claimHolder(t)
		require.True(t, held)
		pointer, objects := f.mustRecord(t).Pointer.Key, f.objectKeys(t)

		_, err := f.m.NewUEFIWriteBackReaper().Sweep(t.Context())
		require.NoError(t, err)

		_, held = f.claimHolder(t)
		assert.False(t, held)
		assert.Equal(t, pointer, f.mustRecord(t).Pointer.Key, "a clean copy is not written back")
		assert.Equal(t, objects, f.objectKeys(t))
	})
}

// A running guest whose reconnect could not read the claim is adopted by the
// reaper, so its changes land while it runs rather than only at the stop.
func TestUEFIWriteBackReaper_AdoptsARunningGuestWhoseReconnectFailed(t *testing.T) {
	f := newUEFIFixture(t)
	flaky := f.flakyClaims()
	instance, _ := f.running(t)
	f.m.uefi.take(testUEFIInstance).halt()
	flaky.failLoads.Store(1)

	f.m.resumeUEFIWriter(instance)
	require.Nil(t, f.m.uefi.get(testUEFIInstance), "the reconnect's claim read failed")

	_, err := f.m.NewUEFIWriteBackReaper().Sweep(t.Context())
	require.NoError(t, err)

	w := f.m.uefi.get(testUEFIInstance)
	require.NotNil(t, w, "the reaper registers a writer for the running guest")
	assert.NotNil(t, w.cancel, "and starts it")
	_, held := f.claimHolder(t)
	assert.True(t, held, "a running guest's claim is never unlocked by the reaper")
}

// A reconnect whose claim read failed retries on its own, so a running guest's
// writer is back within one retry interval rather than at the next reaper sweep.
func TestResumeUEFIWriter_RetriesAFailedClaimReadBeforeTheReaper(t *testing.T) {
	f := newUEFIFixture(t)
	flaky := f.flakyClaims()
	instance, clock := f.running(t)
	f.m.uefi.take(testUEFIInstance).halt()
	flaky.failLoads.Store(1)

	f.m.resumeUEFIWriter(instance)
	require.Nil(t, f.m.uefi.get(testUEFIInstance), "the reconnect's claim read failed")
	clock.waitArmed(t)
	clock.Advance(uefiResumeRetry)

	require.Eventually(t, func() bool { return f.m.uefi.get(testUEFIInstance) != nil },
		10*time.Second, 10*time.Millisecond, "one retry interval registers the writer with no reaper sweep")
	want := f.guestWrites(t, "retried")
	require.Eventually(t, func() bool {
		clock.Advance(uefiWriteBackQuiet)
		return bytes.Equal(want, f.pointedRegion(t))
	}, 10*time.Second, 10*time.Millisecond)
}

// A reconnect retry that finds the claim held by another node stops: that node
// owns the store now, so this one never starts writing it back.
func TestResumeUEFIWriter_StopsRetryingOnceTheClaimIsHeldElsewhere(t *testing.T) {
	f := newUEFIFixture(t)
	flaky := f.flakyClaims()
	instance, clock := f.running(t)
	f.m.uefi.take(testUEFIInstance).halt()
	flaky.failLoads.Store(1)

	f.m.resumeUEFIWriter(instance)
	clock.waitArmed(t)
	old, _ := f.claimHolder(t)
	_, err := f.peerClaim("node-b").TakeOver(t.Context(), old)
	require.NoError(t, err)
	loads := flaky.loads.Load()
	clock.Advance(uefiResumeRetry)
	require.Eventually(t, func() bool { return flaky.loads.Load() > loads }, 10*time.Second, 10*time.Millisecond)
	loads = flaky.loads.Load()

	assert.Never(t, func() bool {
		clock.Advance(uefiResumeRetry)
		return flaky.loads.Load() > loads
	}, 500*time.Millisecond, 10*time.Millisecond, "a claim held elsewhere ends the retries")
	assert.Nil(t, f.m.uefi.get(testUEFIInstance))
}
