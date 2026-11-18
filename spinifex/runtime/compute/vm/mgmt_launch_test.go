package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/qmp"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	mgmtTestNode = "node-a"
	mgmtTestIP   = "10.15.8.10"
	mgmtTestBoot = "boot-1"
)

// fakeMgmtAddresses is the authoritative address record: an address belongs to
// at most one instance.
type fakeMgmtAddresses struct {
	mu       sync.Mutex
	held     map[string]string // ip -> instance
	claims   []string
	confirms []string
	releases []string
	failRel  error
}

func newFakeMgmtAddresses() *fakeMgmtAddresses {
	return &fakeMgmtAddresses{held: map[string]string{}}
}

func (f *fakeMgmtAddresses) Claim(id, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if holder, ok := f.held[ip]; ok && holder != id {
		return fmt.Errorf("%s holds %s", holder, ip)
	}
	f.held[ip] = id
	f.claims = append(f.claims, id)
	return nil
}

func (f *fakeMgmtAddresses) Confirm(id, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held[ip] != id {
		return fmt.Errorf("%s is not reserved for %s", ip, id)
	}
	f.confirms = append(f.confirms, id)
	return nil
}

func (f *fakeMgmtAddresses) Release(id, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRel != nil {
		return f.failRel
	}
	if f.held[ip] == id {
		delete(f.held, ip)
	}
	f.releases = append(f.releases, id)
	return nil
}

// counts reports address writes a launch or teardown made: claims and
// confirmations together, then releases.
func (f *fakeMgmtAddresses) counts() (claims, releases int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.claims) + len(f.confirms), len(f.releases)
}

// mgmtRig is a manager in durable mode over a fake /proc, boot id and runtime dir.
type mgmtRig struct {
	m       *Manager
	addrs   *fakeMgmtAddresses
	cleaner *recordingInstanceCleaner
	dir     string
	proc    string
	boot    string
}

func newMgmtRig(t *testing.T) *mgmtRig {
	t.Helper()
	r := &mgmtRig{addrs: newFakeMgmtAddresses(), cleaner: &recordingInstanceCleaner{},
		dir: t.TempDir(), proc: t.TempDir(), boot: mgmtTestBoot}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	prevRoot, prevBoot := mgmtProcRoot, mgmtBootID
	mgmtProcRoot = r.proc
	mgmtBootID = func() (string, error) { return r.boot, nil }
	t.Cleanup(func() { mgmtProcRoot, mgmtBootID = prevRoot, prevBoot })
	r.m = NewManagerWithDeps(Deps{NodeID: mgmtTestNode, InstanceCleaner: r.cleaner,
		MgmtLaunch: MgmtLaunchDeps{Dir: r.dir, Addresses: r.addrs}})
	return r
}

func mgmtVM(id string) *VM { return &VM{ID: id, MgmtIP: mgmtTestIP} }

// process puts a process in the fake /proc; withPidfile gives it the
// instance's QEMU -pidfile argument.
func (r *mgmtRig) process(t *testing.T, pid int, state byte, start uint64, pidfileOf string) {
	t.Helper()
	require.NoError(t, r.writeProcess(pid, state, start, pidfileOf))
}

// writeProcess is process for a goroutine other than the test's own.
func (r *mgmtRig) writeProcess(pid int, state byte, start uint64, pidfileOf string) error {
	d := filepath.Join(r.proc, strconv.Itoa(pid))
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	fields := []string{string(state)}
	for range 18 {
		fields = append(fields, "0")
	}
	fields = append(fields, strconv.FormatUint(start, 10), "0", "0")
	stat := fmt.Sprintf("%d (qemu-system-x86) %s\n", pid, strings.Join(fields, " "))
	if err := os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o600); err != nil {
		return err
	}
	argv := []string{"qemu-system-x86_64", "-name", "guest"}
	if pidfileOf != "" {
		pf, err := hostprocess.GeneratePidFile(pidfileOf)
		if err != nil {
			return err
		}
		argv = append(argv, "-pidfile", pf)
	}
	return os.WriteFile(filepath.Join(d, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o600)
}

func (r *mgmtRig) put(t *testing.T, rec mgmtLaunchRecord) {
	t.Helper()
	if rec.Schema == 0 {
		rec.Schema = mgmtLaunchSchema
	}
	if rec.Node == "" {
		rec.Node = mgmtTestNode
	}
	if rec.MgmtIP == "" {
		rec.MgmtIP = mgmtTestIP
	}
	if rec.BootID == "" {
		rec.BootID = mgmtTestBoot
	}
	require.NoError(t, writeMgmtLaunch(r.dir, &rec))
}

func (r *mgmtRig) record(t *testing.T, id string) *mgmtLaunchRecord {
	t.Helper()
	rec, err := readMgmtLaunch(r.dir, id)
	require.NoError(t, err)
	return rec
}

// starter is a QEMU start that runs as pid with start time start.
func (r *mgmtRig) starter(t *testing.T, pid int, start uint64, calls *int) func(*VM) error {
	return func(v *VM) error {
		*calls++
		r.process(t, pid, 'S', start, v.ID)
		v.qemuPID.Store(int64(pid))
		return nil
	}
}

func TestMgmtLaunch_NewInstanceRecordsItsRunningProcess(t *testing.T) {
	r := newMgmtRig(t)
	v := mgmtVM("i-new")
	require.NoError(t, r.addrs.Claim(v.ID, mgmtTestIP))
	require.NoError(t, r.m.InitMgmtLaunch(v))
	assert.Equal(t, MgmtLaunchClaimed, r.record(t, v.ID).State)

	calls := 0
	require.NoError(t, r.m.startGuestQEMU(v, r.starter(t, 4001, 777, &calls), false))

	rec := r.record(t, v.ID)
	assert.Equal(t, 1, calls)
	assert.Equal(t, MgmtLaunchRunning, rec.State)
	assert.Equal(t, uint64(1), rec.Generation, "a claimed record's first launch keeps its generation")
	assert.Equal(t, 4001, rec.PID)
	assert.Equal(t, uint64(777), rec.StartTime)
	assert.Equal(t, "i-new", r.addrs.held[mgmtTestIP])
}

func TestMgmtLaunch_RecoveredIDCannotInitializeOverAnExistingRecord(t *testing.T) {
	r := newMgmtRig(t)
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 4, State: MgmtLaunchStarting})

	err := r.m.InitMgmtLaunch(mgmtVM("i-1"))

	require.ErrorIs(t, err, errMgmtLaunchExists)
	assert.Equal(t, uint64(4), r.record(t, "i-1").Generation, "the unresolved record is kept")
}

func TestMgmtLaunch_MissingRecordRefusesTheLaunch(t *testing.T) {
	r := newMgmtRig(t)
	calls := 0

	err := r.m.startGuestQEMU(mgmtVM("i-reused"), r.starter(t, 4001, 1, &calls), false)

	require.ErrorIs(t, err, errMgmtLaunchMissing)
	require.ErrorIs(t, err, errQEMUNotStarted)
	claims, _ := r.addrs.counts()
	assert.Zero(t, calls, "no QEMU may start without a launch record")
	assert.Zero(t, claims)
	_, readErr := readMgmtLaunch(r.dir, "i-reused")
	assert.ErrorIs(t, readErr, errMgmtLaunchMissing, "a refused launch writes no record")
}

func TestMgmtLaunch_AbortBeforeProcessStartRecordsExited(t *testing.T) {
	r := newMgmtRig(t)
	v := mgmtVM("i-1")
	require.NoError(t, r.m.InitMgmtLaunch(v))

	require.NoError(t, r.addrs.Claim(v.ID, mgmtTestIP))
	err := r.m.startGuestQEMU(v, func(*VM) error { return fmt.Errorf("%w: no tap", errQEMUNotStarted) }, false)

	require.ErrorIs(t, err, errQEMUNotStarted)
	assert.Equal(t, MgmtLaunchExited, r.record(t, v.ID).State)
}

// The launch must refuse, and change nothing, whenever the previous
// generation could still be using the address.
func TestMgmtLaunch_UnresolvedPreviousGenerationRefusesTheLaunch(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r *mgmtRig)
	}{
		{"recorded process still running", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 3, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
			r.process(t, 500, 'S', 9, "i-1")
		}},
		{"started with no recorded identity", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 3, State: MgmtLaunchStarting})
		}},
		{"recorded pid gone but another process has the pidfile", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 3, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
			r.process(t, 612, 'S', 40, "i-1")
		}},
		{"a process's argv is unreadable", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 3, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
			require.NoError(t, os.MkdirAll(filepath.Join(r.proc, "613", "cmdline"), 0o700))
		}},
		{"recorded process unreadable", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 3, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
			require.NoError(t, os.MkdirAll(filepath.Join(r.proc, "500", "stat"), 0o700))
		}},
		{"record names another node", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Node: "node-b", Generation: 3, State: MgmtLaunchExited})
		}},
		{"record names another address", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", MgmtIP: "10.15.8.11", Generation: 3, State: MgmtLaunchExited})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newMgmtRig(t)
			tc.setup(t, r)
			before, err := os.ReadFile(filepath.Join(r.dir, "i-1.json"))
			require.NoError(t, err)
			calls := 0

			err = r.m.startGuestQEMU(mgmtVM("i-1"), r.starter(t, 700, 1, &calls), false)

			require.Error(t, err)
			after, readErr := os.ReadFile(filepath.Join(r.dir, "i-1.json"))
			require.NoError(t, readErr)
			claims, releases := r.addrs.counts()
			assert.Zero(t, calls, "no second QEMU may start")
			assert.Zero(t, claims)
			assert.Zero(t, releases)
			assert.Equal(t, string(before), string(after), "the unresolved record is not replaced")
		})
	}
}

func TestMgmtLaunch_CorruptOrUnknownRecordRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"corrupt":        "{not json",
		"unknown schema": `{"schema":2,"instance_id":"i-1","node":"node-a","mgmt_ip":"10.15.8.10","boot_id":"boot-1","generation":1,"state":"exited"}`,
		"unknown field":  `{"schema":1,"instance_id":"i-1","node":"node-a","mgmt_ip":"10.15.8.10","boot_id":"boot-1","generation":1,"state":"exited","x":1}`,
		"unknown state":  `{"schema":1,"instance_id":"i-1","node":"node-a","mgmt_ip":"10.15.8.10","boot_id":"boot-1","generation":1,"state":"paused"}`,
		"trailing data":  `{"schema":1,"instance_id":"i-1","node":"node-a","mgmt_ip":"10.15.8.10","boot_id":"boot-1","generation":1,"state":"exited"}garbage`,
		"trailing brace": `{"schema":1,"instance_id":"i-1","node":"node-a","mgmt_ip":"10.15.8.10","boot_id":"boot-1","generation":1,"state":"exited"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newMgmtRig(t)
			require.NoError(t, os.WriteFile(filepath.Join(r.dir, "i-1.json"), []byte(body), 0o600))
			calls := 0

			require.Error(t, r.m.startGuestQEMU(mgmtVM("i-1"), r.starter(t, 700, 1, &calls), false))
			require.Error(t, r.m.InitMgmtLaunch(mgmtVM("i-1")), "a bad record is not overwritten by a first launch")
			r.m.releaseMgmt(mgmtVM("i-1"), 1)

			_, releases := r.addrs.counts()
			assert.Zero(t, calls)
			assert.Zero(t, releases)
		})
	}
}

// Each is positive evidence that the previous generation stopped.
func TestMgmtLaunch_ProvenStoppedPreviousGenerationAdvances(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r *mgmtRig)
	}{
		{"reaped by this daemon", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 3, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
			r.m.mgmtReaped.note("i-1", 500)
		}},
		{"pid gone and no pidfile holder", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 3, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
		}},
		{"pid reused by another process", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 3, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
			r.process(t, 500, 'S', 10, "")
		}},
		{"zombie awaiting its reaper", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 3, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
			r.process(t, 500, 'Z', 9, "")
		}},
		{"host rebooted since", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", BootID: "boot-0", Generation: 3, State: MgmtLaunchStarting})
		}},
		{"recorded exit", func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 3, State: MgmtLaunchExited})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newMgmtRig(t)
			require.NoError(t, r.addrs.Claim("i-1", mgmtTestIP))
			tc.setup(t, r)
			calls := 0

			require.NoError(t, r.m.startGuestQEMU(mgmtVM("i-1"), r.starter(t, 700, 2, &calls), false))

			rec := r.record(t, "i-1")
			assert.Equal(t, 1, calls)
			assert.Equal(t, uint64(4), rec.Generation)
			assert.Equal(t, MgmtLaunchRunning, rec.State)
			assert.Equal(t, 700, rec.PID)
			assert.Equal(t, mgmtTestBoot, rec.BootID)
		})
	}
}

func TestMgmtLaunch_StartRefusedWhenTheAddressWentToAnotherInstance(t *testing.T) {
	r := newMgmtRig(t)
	r.put(t, mgmtLaunchRecord{InstanceID: "i-a", Generation: 2, State: MgmtLaunchExited})
	require.NoError(t, r.addrs.Claim("i-b", mgmtTestIP))
	calls := 0

	err := r.m.startGuestQEMU(mgmtVM("i-a"), r.starter(t, 700, 1, &calls), true)

	require.Error(t, err)
	assert.Zero(t, calls)
	assert.Equal(t, MgmtLaunchExited, r.record(t, "i-a").State)
	assert.Equal(t, "i-b", r.addrs.held[mgmtTestIP])
}

func TestMgmtRelease_ProvenStoppedGuestReleasesAndKeepsItsGeneration(t *testing.T) {
	r := newMgmtRig(t)
	require.NoError(t, r.addrs.Claim("i-1", mgmtTestIP))
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 5, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
	r.m.mgmtReaped.note("i-1", 500)

	r.m.releaseMgmt(mgmtVM("i-1"), 5)

	rec := r.record(t, "i-1")
	_, releases := r.addrs.counts()
	assert.Equal(t, 1, releases)
	assert.Equal(t, MgmtLaunchExited, rec.State)
	assert.Equal(t, uint64(5), rec.Generation, "the generation is never reset")
	assert.NotContains(t, r.addrs.held, mgmtTestIP)
}

func TestMgmtRelease_HoldsWithoutProof(t *testing.T) {
	cases := []struct {
		name  string
		gen   uint64
		setup func(t *testing.T, r *mgmtRig)
	}{
		{"process still running", 5, func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 5, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
			r.process(t, 500, 'S', 9, "i-1")
		}},
		{"started with no identity", 5, func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 5, State: MgmtLaunchStarting})
		}},
		{"no record", 0, func(*testing.T, *mgmtRig) {}},
		{"cleanup from an earlier launch", 4, func(t *testing.T, r *mgmtRig) {
			r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 5, State: MgmtLaunchExited})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newMgmtRig(t)
			require.NoError(t, r.addrs.Claim("i-1", mgmtTestIP))
			tc.setup(t, r)

			r.m.releaseMgmt(mgmtVM("i-1"), tc.gen)

			_, releases := r.addrs.counts()
			assert.Zero(t, releases, "an uncertain result never releases the address")
			assert.Equal(t, "i-1", r.addrs.held[mgmtTestIP])
		})
	}
}

func TestMgmtRelease_UnacknowledgedReleaseKeepsTheRecordRunning(t *testing.T) {
	r := newMgmtRig(t)
	r.addrs.failRel = errors.New("write outcome unknown")
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 5, State: MgmtLaunchRunning, PID: 500, StartTime: 9})

	r.m.releaseMgmt(mgmtVM("i-1"), 5)

	assert.Equal(t, MgmtLaunchRunning, r.record(t, "i-1").State)
}

// Two VM objects carrying one ID share the guard: a cleanup captured before a
// relaunch waits for it, then finds itself stale and holds the address.
func TestMgmtLaunch_CleanupOnAnotherVMObjectWaitsAndIsStale(t *testing.T) {
	r := newMgmtRig(t)
	require.NoError(t, r.addrs.Claim("i-1", mgmtTestIP))
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 1, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
	r.m.mgmtReaped.note("i-1", 500)

	launching, proceed := make(chan struct{}), make(chan struct{})
	launchDone := make(chan error, 1)
	go func() {
		launchDone <- r.m.startGuestQEMU(mgmtVM("i-1"), func(v *VM) error {
			close(launching)
			<-proceed
			if err := r.writeProcess(700, 'S', 2, v.ID); err != nil {
				return err
			}
			v.qemuPID.Store(700)
			return nil
		}, false)
	}()
	<-launching

	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		r.m.releaseMgmt(mgmtVM("i-1"), 1)
	}()
	select {
	case <-cleanupDone:
		t.Fatal("cleanup ran while the launch held the instance's guard")
	case <-time.After(50 * time.Millisecond):
	}
	close(proceed)
	require.NoError(t, <-launchDone)
	<-cleanupDone

	rec := r.record(t, "i-1")
	_, releases := r.addrs.counts()
	assert.Zero(t, releases, "the earlier launch's cleanup must not release the later launch's address")
	assert.Equal(t, uint64(2), rec.Generation)
	assert.Equal(t, MgmtLaunchRunning, rec.State)
}

func TestMgmtAdopt_IdentifiedSurvivingProcess(t *testing.T) {
	t.Run("no record is adopted as running", func(t *testing.T) {
		r := newMgmtRig(t)
		r.process(t, 800, 'S', 33, "i-1")
		require.NoError(t, hostprocess.WritePidFile("i-1", 800))

		r.m.adoptMgmtLaunch(mgmtVM("i-1"))

		rec := r.record(t, "i-1")
		assert.Equal(t, MgmtLaunchRunning, rec.State)
		assert.Equal(t, uint64(1), rec.Generation)
		assert.Equal(t, 800, rec.PID)
		assert.Equal(t, uint64(33), rec.StartTime)
	})
	t.Run("a starting record keeps its generation", func(t *testing.T) {
		r := newMgmtRig(t)
		r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 6, State: MgmtLaunchStarting})
		r.process(t, 800, 'S', 33, "i-1")
		require.NoError(t, hostprocess.WritePidFile("i-1", 800))

		r.m.adoptMgmtLaunch(mgmtVM("i-1"))

		rec := r.record(t, "i-1")
		assert.Equal(t, MgmtLaunchRunning, rec.State)
		assert.Equal(t, uint64(6), rec.Generation)
	})
	t.Run("an unidentified process is not adopted", func(t *testing.T) {
		r := newMgmtRig(t)
		r.process(t, 800, 'S', 33, "")
		require.NoError(t, hostprocess.WritePidFile("i-1", 800))

		r.m.adoptMgmtLaunch(mgmtVM("i-1"))

		_, err := readMgmtLaunch(r.dir, "i-1")
		assert.ErrorIs(t, err, errMgmtLaunchMissing, "a missing record stays missing, so the address stays held")
	})
	t.Run("a different recorded process is not replaced", func(t *testing.T) {
		r := newMgmtRig(t)
		r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 6, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
		r.process(t, 800, 'S', 33, "i-1")
		require.NoError(t, hostprocess.WritePidFile("i-1", 800))

		r.m.adoptMgmtLaunch(mgmtVM("i-1"))

		assert.Equal(t, 500, r.record(t, "i-1").PID)
	})
}

func TestMgmtLaunch_LegacyModeIsUntouched(t *testing.T) {
	m := NewManagerWithDeps(Deps{})
	calls := 0
	require.NoError(t, m.startGuestQEMU(mgmtVM("i-1"), func(*VM) error { calls++; return nil }, false))
	require.NoError(t, m.InitMgmtLaunch(mgmtVM("i-1")))
	assert.Equal(t, 1, calls)
}

// durableShutdownRig is shutdownTestManager in durable management mode.
func durableShutdownRig(t *testing.T) (*mgmtRig, *recordingInstanceCleaner) {
	t.Helper()
	m, _, _, cleaner, _ := shutdownTestManager(t)
	r := newMgmtRig(t)
	m.deps.NodeID = mgmtTestNode
	m.deps.MgmtLaunch = MgmtLaunchDeps{Dir: r.dir, Addresses: r.addrs}
	r.m = m
	require.NoError(t, r.addrs.Claim("i-1", mgmtTestIP))
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 2, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
	r.m.mgmtReaped.note("i-1", 500)
	return r, cleaner
}

func runningMgmtVM(desired DesiredState) *VM {
	v := mgmtVM("i-1")
	v.Status, v.InstanceType, v.Instance, v.DesiredState = StateRunning, "t3.micro", &ec2.Instance{}, desired
	v.mgmtGen.Store(2)
	return v
}

func TestMgmtTeardown_TerminateReleasesAProvenStoppedGuest(t *testing.T) {
	r, _ := durableShutdownRig(t)
	r.m.Insert(runningMgmtVM(DesiredRunning))

	require.NoError(t, r.m.Terminate("i-1"))

	_, releases := r.addrs.counts()
	assert.Equal(t, 1, releases)
	assert.Equal(t, MgmtLaunchExited, r.record(t, "i-1").State)
}

func TestMgmtTeardown_TerminateHoldsARunningGuest(t *testing.T) {
	r, _ := durableShutdownRig(t)
	r.m.mgmtReaped = mgmtReaped{}
	r.process(t, 500, 'S', 9, "i-1")
	r.m.Insert(runningMgmtVM(DesiredRunning))

	require.NoError(t, r.m.Terminate("i-1"))

	_, releases := r.addrs.counts()
	assert.Zero(t, releases, "a guest that may still run keeps its address")
	assert.Equal(t, "i-1", r.addrs.held[mgmtTestIP])
}

func TestMgmtTeardown_OperatorStopReleasesAndDrainStopKeeps(t *testing.T) {
	t.Run("operator stop", func(t *testing.T) {
		r, _ := durableShutdownRig(t)
		r.m.Insert(runningMgmtVM(DesiredStopped))

		require.NoError(t, r.m.Stop("i-1"))

		_, releases := r.addrs.counts()
		assert.Equal(t, 1, releases)
	})
	t.Run("drain stop", func(t *testing.T) {
		r, _ := durableShutdownRig(t)
		r.m.Insert(runningMgmtVM(DesiredRunning))

		require.NoError(t, r.m.StopAll())

		_, releases := r.addrs.counts()
		assert.Zero(t, releases, "a drained guest is relaunched on its reserved address")
		assert.Equal(t, "i-1", r.addrs.held[mgmtTestIP])
	})
}

func TestMgmtTeardown_RecoveryFailureKeepsTheReservation(t *testing.T) {
	r, _ := durableShutdownRig(t)
	v := runningMgmtVM(DesiredRunning)
	r.m.Insert(v)

	r.m.MarkRecoveryFailed(v, "test")
	r.m.WaitForBackgroundWork()

	_, releases := r.addrs.counts()
	assert.Zero(t, releases)
}

// The reboot path starts QEMU without launch(); it must take the same guard,
// prove the old process ended and record the new one.
func TestMgmtReboot_RecordsTheReplacementProcess(t *testing.T) {
	r := newMgmtRig(t)
	rr := newRebootRig(t, Deps{NodeID: mgmtTestNode, MgmtLaunch: MgmtLaunchDeps{Dir: r.dir, Addresses: r.addrs}}, "x", true)
	rr.vm.MgmtIP = mgmtTestIP
	rr.vm.mgmtGen.Store(2)
	r.m = rr.m
	require.NoError(t, r.addrs.Claim("i-1", mgmtTestIP))
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 2, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
	inner := startRelaunchedQEMU
	startRelaunchedQEMU = func(m *Manager, v *VM) error {
		if err := inner(m, v); err != nil {
			return err
		}
		r.process(t, 701, 'S', 50, v.ID)
		v.qemuPID.Store(701)
		return nil
	}

	rr.reboot(t)

	rec := r.record(t, "i-1")
	assert.Equal(t, uint64(3), rec.Generation)
	assert.Equal(t, MgmtLaunchRunning, rec.State)
	assert.Equal(t, 701, rec.PID)
}

func TestMgmtReboot_RefusedWhileTheOldProcessIsUnresolved(t *testing.T) {
	r := newMgmtRig(t)
	rr := newRebootRig(t, Deps{NodeID: mgmtTestNode, MgmtLaunch: MgmtLaunchDeps{Dir: r.dir, Addresses: r.addrs}}, "x", true)
	rr.vm.MgmtIP = mgmtTestIP
	rr.vm.mgmtGen.Store(2)
	r.m = rr.m
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 2, State: MgmtLaunchStarting})

	rr.reboot(t)

	assert.Equal(t, 1, rr.qemu.generation(), "no replacement QEMU starts over an unresolved launch")
	assert.Equal(t, uint64(2), r.record(t, "i-1").Generation)
}

// Only an explicit start may recreate a missing reservation; restore, crash
// restart and reboot re-confirm the one they had.
func TestMgmtLaunch_OnlyAFreshStartRecreatesAMissingReservation(t *testing.T) {
	r := newMgmtRig(t)
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 2, State: MgmtLaunchExited})
	calls := 0

	require.Error(t, r.m.startGuestQEMU(mgmtVM("i-1"), r.starter(t, 700, 1, &calls), false))
	assert.Zero(t, calls)
	assert.Equal(t, uint64(2), r.record(t, "i-1").Generation)

	require.NoError(t, r.m.startGuestQEMU(mgmtVM("i-1"), r.starter(t, 700, 1, &calls), true))
	assert.Equal(t, 1, calls)
	assert.Equal(t, "i-1", r.addrs.held[mgmtTestIP])
}

// An old VM object's stop, captured before a newer launch on another object
// with the same ID, waits for that launch and then leaves its process, taps
// and address alone.
func TestMgmtTeardown_OldObjectStopLeavesTheNewerLaunchAlone(t *testing.T) {
	r := newMgmtRig(t)
	require.NoError(t, r.addrs.Claim("i-1", mgmtTestIP))
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 1, State: MgmtLaunchExited})
	old := mgmtVM("i-1")
	old.mgmtGen.Store(1)
	oldGen := r.m.mgmtGeneration(old)

	launching, proceed := make(chan struct{}), make(chan struct{})
	launchDone := make(chan error, 1)
	go func() {
		launchDone <- r.m.startGuestQEMU(mgmtVM("i-1"), func(v *VM) error {
			close(launching)
			<-proceed
			if err := r.writeProcess(700, 'S', 2, v.ID); err != nil {
				return err
			}
			v.qemuPID.Store(700)
			return nil
		}, false)
	}()
	<-launching

	stopDone := make(chan error, 1)
	go func() { stopDone <- r.m.stopCleanup(old, oldGen, true) }()
	select {
	case <-stopDone:
		t.Fatal("the old object's stop ran while the newer launch held the guard")
	case <-time.After(50 * time.Millisecond):
	}
	close(proceed)
	require.NoError(t, <-launchDone)
	require.ErrorIs(t, <-stopDone, errTeardownSuperseded)

	_, releases := r.addrs.counts()
	assert.Zero(t, releases)
	assert.Empty(t, r.cleaner.cleanupMgmt, "the newer launch's management tap is not removed")
	assert.Equal(t, MgmtLaunchRunning, r.record(t, "i-1").State)
}

// A cleanup on another VM object that lands while a reboot is between ending
// the old QEMU and starting its replacement waits, then finds itself stale.
func TestMgmtReboot_CleanupInTheRelaunchGapWaitsAndIsStale(t *testing.T) {
	r := newMgmtRig(t)
	rr := newRebootRig(t, Deps{NodeID: mgmtTestNode, InstanceCleaner: r.cleaner,
		MgmtLaunch: MgmtLaunchDeps{Dir: r.dir, Addresses: r.addrs}}, "x", true)
	rr.vm.MgmtIP = mgmtTestIP
	rr.vm.mgmtGen.Store(2)
	r.m = rr.m
	require.NoError(t, r.addrs.Claim("i-1", mgmtTestIP))
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 2, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
	inner := startRelaunchedQEMU
	startRelaunchedQEMU = func(m *Manager, v *VM) error {
		if err := inner(m, v); err != nil {
			return err
		}
		r.process(t, 701, 'S', 50, v.ID)
		v.qemuPID.Store(701)
		return nil
	}

	other := mgmtVM("i-1")
	cleanupDone := make(chan error, 1)
	var started sync.Once
	rr.qemu.onCommand = func(_ int, cmd qmp.QMPCommand) {
		if cmd.Execute == "quit" {
			started.Do(func() { go func() { cleanupDone <- r.m.stopCleanup(other, 2, true) }() })
		}
	}

	rr.reboot(t)
	require.ErrorIs(t, <-cleanupDone, errTeardownSuperseded)

	rec := r.record(t, "i-1")
	_, releases := r.addrs.counts()
	assert.Equal(t, uint64(3), rec.Generation)
	assert.Equal(t, 701, rec.PID)
	assert.Zero(t, releases, "the stale cleanup must not release the rebooted guest's address")
	assert.Empty(t, r.cleaner.cleanupMgmt, "the stale cleanup must not remove the rebooted guest's tap")
}

// The old object's teardown starts only after the replacement's launch is
// already recorded running: its own binding, not the marker, decides.
func TestMgmtTeardown_OldObjectTeardownAfterTheReplacementIsRunning(t *testing.T) {
	r := newMgmtRig(t)
	require.NoError(t, r.addrs.Claim("i-1", mgmtTestIP))
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 1, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
	old := mgmtVM("i-1")
	r.m.bindMgmtLaunch(old)
	r.m.Insert(old)
	r.m.mgmtReaped.note("i-1", 500)

	replacement := mgmtVM("i-1")
	calls := 0
	require.NoError(t, r.m.startGuestQEMU(replacement, r.starter(t, 700, 2, &calls), false))
	r.m.Insert(replacement)
	require.Equal(t, uint64(2), r.record(t, "i-1").Generation)

	r.m.ForgetSuperseded(old)

	_, releases := r.addrs.counts()
	got, ok := r.m.Get("i-1")
	assert.True(t, ok && got == replacement, "the replacement stays in the running set")
	assert.Zero(t, releases)
	assert.Empty(t, r.cleaner.cleanupMgmt)
	assert.Equal(t, MgmtLaunchRunning, r.record(t, "i-1").State)
}

func TestMgmtTeardown_UnboundObjectHolds(t *testing.T) {
	r := newMgmtRig(t)
	require.NoError(t, r.addrs.Claim("i-1", mgmtTestIP))
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 3, State: MgmtLaunchExited})
	unbound := mgmtVM("i-1")

	require.ErrorIs(t, r.m.stopCleanup(unbound, r.m.mgmtGeneration(unbound), true), errTeardownSuperseded)

	_, releases := r.addrs.counts()
	assert.Zero(t, releases)
	assert.Empty(t, r.cleaner.cleanupMgmt)
}

func TestMgmtRestore_BindsLoadedObjectsToTheirLaunch(t *testing.T) {
	r := newMgmtRig(t)
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 7, State: MgmtLaunchExited})
	loaded := mgmtVM("i-1")

	r.m.bindMgmtLaunch(loaded)

	assert.Equal(t, uint64(7), r.m.mgmtGeneration(loaded))
}

// A superseded VM object's reboot would end the replacement through the shared
// pidfile, so it is refused before any process is touched.
func TestMgmtReboot_StaleObjectLeavesTheCurrentGuestUntouched(t *testing.T) {
	r := newMgmtRig(t)
	rr := newRebootRig(t, Deps{NodeID: mgmtTestNode, InstanceCleaner: r.cleaner,
		MgmtLaunch: MgmtLaunchDeps{Dir: r.dir, Addresses: r.addrs}}, "x", true)
	rr.vm.MgmtIP = mgmtTestIP
	rr.vm.mgmtGen.Store(1)
	r.m = rr.m
	require.NoError(t, r.addrs.Claim("i-1", mgmtTestIP))
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 2, State: MgmtLaunchRunning, PID: 701, StartTime: 50})
	r.process(t, 701, 'S', 50, "i-1")
	var quits atomic.Int32
	rr.qemu.onCommand = func(_ int, cmd qmp.QMPCommand) {
		if cmd.Execute == "quit" {
			quits.Add(1)
		}
	}

	rr.reboot(t)

	rec := r.record(t, "i-1")
	claims, releases := r.addrs.counts()
	assert.Zero(t, quits.Load(), "the stale object must not end the current process")
	assert.Equal(t, 1, rr.qemu.generation(), "no replacement QEMU starts")
	assert.Equal(t, uint64(2), rec.Generation)
	assert.Equal(t, 701, rec.PID)
	assert.Equal(t, 1, claims)
	assert.Zero(t, releases)
	assert.Empty(t, r.cleaner.cleanupMgmt)
}

// supersededRig holds an old VM object bound to launch 1 while launch 2, a
// replacement on another object, is running.
func supersededRig(t *testing.T, desired DesiredState) (*mgmtRig, *recordingInstanceCleaner, *fakeStateStore, *VM) {
	t.Helper()
	r, cleaner := durableShutdownRig(t)
	r.put(t, mgmtLaunchRecord{InstanceID: "i-1", Generation: 2, State: MgmtLaunchRunning, PID: 701, StartTime: 50})
	r.process(t, 701, 'S', 50, "i-1")
	old := runningMgmtVM(desired)
	old.mgmtGen.Store(1)
	old.FirstLaunch = true
	old.ENIId, old.PublicIP = "eni-1", "203.0.113.5"
	return r, cleaner, r.m.deps.StateStore.(*fakeStateStore), old
}

func assertReplacementUntouched(t *testing.T, r *mgmtRig, cleaner *recordingInstanceCleaner, store *fakeStateStore) {
	t.Helper()
	_, releases := r.addrs.counts()
	assert.Zero(t, releases)
	rec := r.record(t, "i-1")
	assert.Equal(t, uint64(2), rec.Generation)
	assert.Equal(t, MgmtLaunchRunning, rec.State)
	store.mu.Lock()
	assert.Empty(t, store.terminated, "no terminated record may cover the replacement")
	assert.Empty(t, store.stopped, "no stopped record may cover the replacement")
	store.mu.Unlock()
	cleaner.mu.Lock()
	defer cleaner.mu.Unlock()
	assert.Empty(t, cleaner.cleanupMgmt)
	assert.Empty(t, cleaner.releaseGPU)
	assert.Empty(t, cleaner.detachAndDeleteENI)
	assert.Empty(t, cleaner.releasePublicIP)
	assert.Empty(t, cleaner.deleteVolumes)
	assert.Empty(t, cleaner.releaseAutoAssigned)
}

func TestMgmtSuperseded_TerminateRefusesToFinalize(t *testing.T) {
	r, cleaner, store, old := supersededRig(t, DesiredRunning)
	r.m.Insert(old)

	require.ErrorIs(t, r.m.Terminate("i-1"), errTeardownSuperseded)

	assertReplacementUntouched(t, r, cleaner, store)
}

func TestMgmtSuperseded_OperatorStopRefusesToMigrate(t *testing.T) {
	r, cleaner, store, old := supersededRig(t, DesiredStopped)
	r.m.Insert(old)

	require.ErrorIs(t, r.m.Stop("i-1"), errTeardownSuperseded)

	assertReplacementUntouched(t, r, cleaner, store)
}

func TestMgmtSuperseded_MarkFailedLeavesTheReplacementRunning(t *testing.T) {
	r, cleaner, store, old := supersededRig(t, DesiredRunning)
	replacement := runningMgmtVM(DesiredRunning)
	r.m.Insert(replacement)

	r.m.MarkFailed(t.Context(), old, "launch failed")
	r.m.WaitForBackgroundWork()

	assertReplacementUntouched(t, r, cleaner, store)
	got, ok := r.m.Get("i-1")
	assert.True(t, ok && got == replacement)
	assert.Equal(t, StateRunning, r.m.Status(replacement))
}

func TestMgmtSuperseded_RecoveryFailureLeavesTheReplacementRunning(t *testing.T) {
	r, cleaner, store, old := supersededRig(t, DesiredRunning)
	replacement := runningMgmtVM(DesiredRunning)
	r.m.Insert(replacement)

	r.m.MarkRecoveryFailed(old, "recovery failed")
	r.m.WaitForBackgroundWork()

	assertReplacementUntouched(t, r, cleaner, store)
	assert.Equal(t, StateRunning, r.m.Status(replacement))
}

// A record for another node or address is not this VM's launch: it binds
// nothing, and neither teardown nor release acts on it.
func TestMgmtLaunch_RecordForAnotherNodeOrAddressHolds(t *testing.T) {
	for name, rec := range map[string]mgmtLaunchRecord{
		"node":    {InstanceID: "i-1", Node: "node-b", Generation: 4, State: MgmtLaunchExited},
		"address": {InstanceID: "i-1", MgmtIP: "10.15.8.99", Generation: 4, State: MgmtLaunchExited},
	} {
		t.Run(name, func(t *testing.T) {
			r := newMgmtRig(t)
			require.NoError(t, r.addrs.Claim("i-1", mgmtTestIP))
			r.put(t, rec)
			v := mgmtVM("i-1")

			r.m.bindMgmtLaunch(v)
			assert.Zero(t, r.m.mgmtGeneration(v), "a mismatched record binds nothing")

			v.mgmtGen.Store(4)
			require.ErrorIs(t, r.m.stopCleanup(v, 4, true), errTeardownSuperseded)
			r.m.releaseMgmt(v, 4)

			_, releases := r.addrs.counts()
			assert.Zero(t, releases)
			assert.Empty(t, r.cleaner.cleanupMgmt)
		})
	}
}

// A legacy guest the operator attested stopped gets an exited first record, so
// a customer start can claim its address and launch generation 2.
func TestMgmtLegacyStopped_AttestedRecordLetsTheStartProceed(t *testing.T) {
	r := newMgmtRig(t)
	calls := 0

	written, err := RecordStoppedMgmtLaunch(r.dir, mgmtTestNode, "i-legacy", mgmtTestIP)
	require.NoError(t, err)
	require.True(t, written)
	require.NoError(t, r.m.startGuestQEMU(mgmtVM("i-legacy"), r.starter(t, 700, 2, &calls), true))

	assert.Equal(t, 1, calls)
	assert.Equal(t, uint64(2), r.record(t, "i-legacy").Generation)
	assert.Equal(t, "i-legacy", r.addrs.held[mgmtTestIP])
}

// An existing record keeps its generation; one for another node or address
// is refused and left as it was.
func TestMgmtLegacyStopped_ExistingRecordIsKeptOrRefused(t *testing.T) {
	r := newMgmtRig(t)
	r.put(t, mgmtLaunchRecord{InstanceID: "i-kept", Generation: 5, State: MgmtLaunchRunning, PID: 500, StartTime: 9})
	r.put(t, mgmtLaunchRecord{InstanceID: "i-moved", Node: "node-other", Generation: 2, State: MgmtLaunchExited})

	written, err := RecordStoppedMgmtLaunch(r.dir, mgmtTestNode, "i-kept", mgmtTestIP)
	require.NoError(t, err)
	assert.False(t, written)
	_, err = RecordStoppedMgmtLaunch(r.dir, mgmtTestNode, "i-moved", mgmtTestIP)
	require.ErrorIs(t, err, errMgmtLaunchMismatch)

	assert.Equal(t, uint64(5), r.record(t, "i-kept").Generation)
	assert.Equal(t, MgmtLaunchRunning, r.record(t, "i-kept").State)
	assert.Equal(t, "node-other", r.record(t, "i-moved").Node)
}
