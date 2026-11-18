package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
)

// mgmtLaunchSchema is the only launch record schema this code reads or writes.
const mgmtLaunchSchema = 1

// MgmtLaunchState is how far an instance's latest launch got toward a QEMU
// process that may use its management address.
type MgmtLaunchState string

const (
	// MgmtLaunchClaimed: the reservation is held and no QEMU was started.
	MgmtLaunchClaimed MgmtLaunchState = "claimed"
	// MgmtLaunchStarting: QEMU may have been started with no identity recorded.
	MgmtLaunchStarting MgmtLaunchState = "starting"
	// MgmtLaunchRunning: QEMU was started as the recorded process.
	MgmtLaunchRunning MgmtLaunchState = "running"
	// MgmtLaunchExited: that launch's process is known to have exited.
	MgmtLaunchExited MgmtLaunchState = "exited"
)

var (
	errMgmtLaunchMissing    = errors.New("no management launch record")
	errMgmtLaunchExists     = errors.New("management launch record already exists")
	errMgmtLaunchMismatch   = errors.New("management launch record names another node or address")
	errMgmtLaunchUnresolved = errors.New("previous launch is not proven stopped")
	errMgmtLaunchStale      = errors.New("VM object is not bound to the latest management launch")
	errTeardownSuperseded   = errors.New("teardown skipped: VM object does not own the latest management launch")
)

// MgmtAddresses is the authoritative record of management addresses.
type MgmtAddresses interface {
	// Claim reserves ip for the instance on this node, creating the entry when
	// the address is free and refusing one another reservation holds.
	Claim(instanceID, ip string) error
	// Confirm re-acknowledges this node's existing reservation of ip for the
	// instance and refuses a missing one.
	Confirm(instanceID, ip string) error
	// Release removes this node's reservation of ip for the instance, and
	// returns only once the removal is acknowledged.
	Release(instanceID, ip string) error
}

// MgmtLaunchDeps enables launch identity for management addresses. With Dir
// empty the manager neither claims nor releases them; the cleaner releases.
type MgmtLaunchDeps struct {
	Dir       string
	Addresses MgmtAddresses
}

// mgmtLaunchRecord is the per-instance local launch record, kept after exit so
// the generation never restarts.
type mgmtLaunchRecord struct {
	Schema     int             `json:"schema"`
	InstanceID string          `json:"instance_id"`
	Node       string          `json:"node"`
	MgmtIP     string          `json:"mgmt_ip"`
	BootID     string          `json:"boot_id"`
	Generation uint64          `json:"generation"`
	State      MgmtLaunchState `json:"state"`
	PID        int             `json:"pid,omitempty"`
	StartTime  uint64          `json:"start_time,omitempty"`
	// ReleasePending records a proven-stopped launch whose address release is
	// not yet acknowledged, so a retry can finish it after a failure or restart.
	ReleasePending bool `json:"release_pending,omitempty"`
}

// Host probes, overridden in tests.
var (
	mgmtProcRoot = "/proc"
	mgmtBootID   = func() (string, error) {
		b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err != nil {
			return "", err
		}
		id := strings.TrimSpace(string(b))
		if id == "" {
			return "", errors.New("empty boot id")
		}
		return id, nil
	}
)

// mgmtReaped records QEMU processes this daemon waited on, by instance: the
// in-process proof that a recorded PID exited.
type mgmtReaped struct {
	mu   sync.Mutex
	pids map[string]int
}

func (r *mgmtReaped) note(id string, pid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pids == nil {
		r.pids = make(map[string]int)
	}
	r.pids[id] = pid
}

func (r *mgmtReaped) has(id string, pid int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return pid > 0 && r.pids[id] == pid
}

func (m *Manager) mgmtDurable() bool { return m.deps.MgmtLaunch.Dir != "" }

func mgmtLaunchPath(dir, id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return "", fmt.Errorf("invalid instance id %q", id)
	}
	return filepath.Join(dir, id+".json"), nil
}

// readMgmtLaunch returns the instance's record. A missing record is
// errMgmtLaunchMissing; an unreadable, corrupt or foreign one is an error.
func readMgmtLaunch(dir, id string) (*mgmtLaunchRecord, error) {
	path, err := mgmtLaunchPath(dir, id)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w for %s", errMgmtLaunchMissing, id)
	}
	if err != nil {
		return nil, fmt.Errorf("read management launch record for %s: %w", id, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var rec mgmtLaunchRecord
	if err := dec.Decode(&rec); err != nil {
		return nil, fmt.Errorf("decode management launch record for %s: %w", id, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode management launch record for %s: trailing data after the record", id)
	}
	if err := rec.validate(id); err != nil {
		return nil, fmt.Errorf("management launch record for %s: %w", id, err)
	}
	return &rec, nil
}

// mgmtRecordMatches refuses a record that is not this node's launch of the
// VM's address.
func (m *Manager) mgmtRecordMatches(rec *mgmtLaunchRecord, instance *VM) error {
	if rec.Node != m.deps.NodeID || rec.MgmtIP != instance.MgmtIP {
		return fmt.Errorf("%w: %s at %s on %s", errMgmtLaunchMismatch, instance.ID, rec.MgmtIP, rec.Node)
	}
	return nil
}

func (r *mgmtLaunchRecord) validate(id string) error {
	switch {
	case r.Schema != mgmtLaunchSchema:
		return fmt.Errorf("unsupported schema %d", r.Schema)
	case r.InstanceID != id:
		return fmt.Errorf("names instance %q", r.InstanceID)
	case r.Node == "" || r.MgmtIP == "" || r.BootID == "" || r.Generation == 0:
		return errors.New("missing node, address, boot or generation")
	}
	switch r.State {
	case MgmtLaunchClaimed, MgmtLaunchStarting, MgmtLaunchExited:
	case MgmtLaunchRunning:
		if r.PID <= 0 {
			return errors.New("running without a pid")
		}
	default:
		return fmt.Errorf("unknown state %q", r.State)
	}
	return nil
}

// writeMgmtLaunch replaces the record durably: temp file, fsync, rename, then
// fsync of the directory. Any error leaves the outcome uncertain.
func writeMgmtLaunch(dir string, rec *mgmtLaunchRecord) error {
	if err := rec.validate(rec.InstanceID); err != nil {
		return err
	}
	path, err := mgmtLaunchPath(dir, rec.InstanceID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, rec.InstanceID+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// procStat reads a process's state and start time (fields 3 and 22 of
// /proc/<pid>/stat).
func procStat(root string, pid int) (state byte, start uint64, err error) {
	raw, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, err
	}
	_, rest, ok := bytes.CutLast(raw, []byte(")"))
	if !ok {
		return 0, 0, errors.New("malformed stat")
	}
	fields := strings.Fields(string(rest))
	if len(fields) < 20 || len(fields[0]) != 1 {
		return 0, 0, errors.New("short stat")
	}
	start, err = strconv.ParseUint(fields[19], 10, 64)
	return fields[0][0], start, err
}

// pidfileHolders lists live processes, of any name, whose argv passes pidfile
// to -pidfile. A process whose argv cannot be read is an error, not absent.
func pidfileHolders(root, pidfile string) ([]int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var holders []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "cmdline"))
		if errors.Is(err, fs.ErrNotExist) {
			continue // exited during the scan
		}
		if err != nil {
			return nil, fmt.Errorf("read argv of pid %d: %w", pid, err)
		}
		args := strings.Split(string(raw), "\x00")
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-pidfile" && args[i+1] == pidfile {
				holders = append(holders, pid)
				break
			}
		}
	}
	return holders, nil
}

// mgmtProvenStopped returns nil only when rec's launch can no longer be using
// its address; any other answer is a reason to hold it.
func (m *Manager) mgmtProvenStopped(rec *mgmtLaunchRecord) error {
	boot, err := mgmtBootID()
	if err != nil {
		return fmt.Errorf("read boot id: %w", err)
	}
	if rec.BootID != boot {
		return nil
	}
	switch rec.State {
	case MgmtLaunchClaimed, MgmtLaunchExited:
		return nil
	case MgmtLaunchStarting:
		return fmt.Errorf("generation %d may have started QEMU with no recorded identity", rec.Generation)
	case MgmtLaunchRunning:
	default:
		return fmt.Errorf("unknown state %q", rec.State)
	}
	if !m.mgmtReaped.has(rec.InstanceID, rec.PID) {
		state, st, err := procStat(mgmtProcRoot, rec.PID)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return fmt.Errorf("read pid %d: %w", rec.PID, err)
		case state == 'Z' || state == 'X':
			// Exited and awaiting its reaper; it holds no devices.
		case rec.StartTime == 0 || st == rec.StartTime:
			return fmt.Errorf("generation %d process %d is still running", rec.Generation, rec.PID)
		}
	}
	pidfile, err := hostprocess.GeneratePidFile(rec.InstanceID)
	if err != nil {
		return err
	}
	holders, err := pidfileHolders(mgmtProcRoot, pidfile)
	if err != nil {
		return err
	}
	if len(holders) > 0 {
		return fmt.Errorf("processes %v still run with %s", holders, pidfile)
	}
	return nil
}

// lockMgmt takes the instance's launch guard, or nothing when the instance has
// no management launch identity. The guard comes before attachMu and relaunchMu.
func (m *Manager) lockMgmt(instance *VM) (unlock func()) {
	if !m.mgmtDurable() || instance.MgmtIP == "" {
		return func() {}
	}
	return m.mgmtLocks.lock(instance.ID)
}

// mgmtLaunchWired refuses a durable launch on a node with no address record,
// before anything is mounted or started.
func (m *Manager) mgmtLaunchWired(instance *VM) error {
	if m.mgmtDurable() && instance.MgmtIP != "" && m.deps.MgmtLaunch.Addresses == nil {
		return fmt.Errorf("%w: %s: management address record not wired", errQEMUNotStarted, instance.ID)
	}
	return nil
}

// startGuestQEMU runs start under the instance's launch guard, between proof
// that the previous launch stopped and the record of what this one became.
func (m *Manager) startGuestQEMU(instance *VM, start func(*VM) error, fresh bool) error {
	unlock := m.lockMgmt(instance)
	defer unlock()
	return m.startGuestQEMULocked(instance, start, fresh)
}

// startGuestQEMULocked is startGuestQEMU for a caller holding the guard.
func (m *Manager) startGuestQEMULocked(instance *VM, start func(*VM) error, fresh bool) error {
	if !m.mgmtDurable() || instance.MgmtIP == "" {
		return start(instance)
	}
	rec, err := m.beginMgmtLaunch(instance, fresh)
	if err != nil {
		return fmt.Errorf("%w: %w", errQEMUNotStarted, err)
	}
	startErr := start(instance)
	m.finishMgmtLaunch(instance, rec, startErr)
	return startErr
}

// identifyQEMU returns the live process QEMU's pidfile names for the instance,
// only when its argv passes that pidfile, with its start time.
func identifyQEMU(id string) (pid int, start uint64, err error) {
	pidfile, err := hostprocess.GeneratePidFile(id)
	if err != nil {
		return 0, 0, err
	}
	pid, err = hostprocess.ReadPidFile(id)
	if err != nil || pid <= 0 {
		return 0, 0, fmt.Errorf("no QEMU pid for %s: %w", id, err)
	}
	state, start, err := procStat(mgmtProcRoot, pid)
	if err != nil {
		return 0, 0, err
	}
	if state == 'Z' || state == 'X' {
		return 0, 0, fmt.Errorf("pid %d has exited", pid)
	}
	holders, err := pidfileHolders(mgmtProcRoot, pidfile)
	if err != nil {
		return 0, 0, err
	}
	if !slices.Contains(holders, pid) {
		return 0, 0, fmt.Errorf("pid %d does not run with %s", pid, pidfile)
	}
	return pid, start, nil
}

// adoptMgmtLaunch records the identity of a surviving QEMU reconnected after a
// restart. A launch with no record, or one caught starting, is adopted only
// when the process is positively identified; anything else stays held.
func (m *Manager) adoptMgmtLaunch(instance *VM) {
	if !m.mgmtDurable() || instance.MgmtIP == "" {
		return
	}
	unlock := m.mgmtLocks.lock(instance.ID)
	defer unlock()
	hold := func(reason string, err error) {
		slog.Warn("Surviving QEMU not adopted; its management address stays held",
			"instanceId", instance.ID, "ip", instance.MgmtIP, "reason", reason, "err", err)
	}
	dir := m.deps.MgmtLaunch.Dir
	rec, err := readMgmtLaunch(dir, instance.ID)
	missing := errors.Is(err, errMgmtLaunchMissing)
	if err != nil && !missing {
		hold("launch record unreadable", err)
		return
	}
	pid, start, err := identifyQEMU(instance.ID)
	if err != nil {
		hold("process not identified", err)
		return
	}
	boot, err := mgmtBootID()
	if err != nil {
		hold("boot id unreadable", err)
		return
	}
	next := mgmtLaunchRecord{Schema: mgmtLaunchSchema, InstanceID: instance.ID, Node: m.deps.NodeID,
		MgmtIP: instance.MgmtIP, BootID: boot, Generation: 1, State: MgmtLaunchRunning, PID: pid, StartTime: start}
	if !missing {
		switch {
		case m.mgmtRecordMatches(rec, instance) != nil:
			hold("launch record names another node or address", m.mgmtRecordMatches(rec, instance))
			return
		case rec.State == MgmtLaunchRunning && rec.PID == pid && rec.BootID == boot && (rec.StartTime == 0 || rec.StartTime == start):
			instance.mgmtGen.Store(rec.Generation)
			return
		case rec.State != MgmtLaunchStarting || rec.BootID != boot:
			hold("process does not match the recorded launch", fmt.Errorf("record is %s pid %d", rec.State, rec.PID))
			return
		}
		next.Generation = rec.Generation
	}
	if err := writeMgmtLaunch(dir, &next); err != nil {
		hold("record write failed", err)
		return
	}
	instance.mgmtGen.Store(next.Generation)
}

// InitMgmtLaunch writes a new instance's first launch record. The caller has
// just had a new reservation of instance.MgmtIP acknowledged; an existing
// record for the ID is refused, so a reused or recovered ID cannot start over.
func (m *Manager) InitMgmtLaunch(instance *VM) error {
	if !m.mgmtDurable() {
		return nil
	}
	unlock := m.mgmtLocks.lock(instance.ID)
	defer unlock()
	_, err := readMgmtLaunch(m.deps.MgmtLaunch.Dir, instance.ID)
	if err == nil {
		return fmt.Errorf("%w for %s", errMgmtLaunchExists, instance.ID)
	}
	if !errors.Is(err, errMgmtLaunchMissing) {
		return err
	}
	boot, err := mgmtBootID()
	if err != nil {
		return fmt.Errorf("read boot id: %w", err)
	}
	if err := writeMgmtLaunch(m.deps.MgmtLaunch.Dir, &mgmtLaunchRecord{
		Schema: mgmtLaunchSchema, InstanceID: instance.ID, Node: m.deps.NodeID,
		MgmtIP: instance.MgmtIP, BootID: boot, Generation: 1, State: MgmtLaunchClaimed,
	}); err != nil {
		return err
	}
	instance.mgmtGen.Store(1)
	return nil
}

// RecordStoppedMgmtLaunch writes an exited first record for a legacy guest the
// operator attests is stopped with its old launchers quiesced. A record for the
// same node and address is kept; one naming another node or address refuses.
func RecordStoppedMgmtLaunch(dir, node, instanceID, ip string) (written bool, err error) {
	rec, err := readMgmtLaunch(dir, instanceID)
	if err == nil {
		if rec.Node != node || rec.MgmtIP != ip {
			return false, fmt.Errorf("%w: %s at %s on %s", errMgmtLaunchMismatch, instanceID, rec.MgmtIP, rec.Node)
		}
		return false, nil
	}
	if !errors.Is(err, errMgmtLaunchMissing) {
		return false, err
	}
	boot, err := mgmtBootID()
	if err != nil {
		return false, fmt.Errorf("read boot id: %w", err)
	}
	if err := writeMgmtLaunch(dir, &mgmtLaunchRecord{
		Schema: mgmtLaunchSchema, InstanceID: instanceID, Node: node,
		MgmtIP: ip, BootID: boot, Generation: 1, State: MgmtLaunchExited,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// bindMgmtLaunch binds an object loaded from saved state to its instance's
// current launch. Restore calls it before anything can launch a replacement.
func (m *Manager) bindMgmtLaunch(instance *VM) {
	if !m.mgmtDurable() || instance.MgmtIP == "" {
		return
	}
	unlock := m.mgmtLocks.lock(instance.ID)
	defer unlock()
	rec, err := readMgmtLaunch(m.deps.MgmtLaunch.Dir, instance.ID)
	if err != nil {
		slog.Warn("Restored instance has no readable launch record; its management address stays held",
			"instanceId", instance.ID, "err", err)
		return
	}
	if err := m.mgmtRecordMatches(rec, instance); err != nil {
		slog.Error("Restored instance's launch record names another node or address; left unbound",
			"instanceId", instance.ID, "err", err)
		return
	}
	instance.mgmtGen.Store(rec.Generation)
}

// beginMgmtLaunch proves the previous generation stopped, claims the address
// and records the new generation as starting. Only a fresh start may create a
// missing reservation. Called with the guard held; an error changes nothing.
func (m *Manager) beginMgmtLaunch(instance *VM, fresh bool) (*mgmtLaunchRecord, error) {
	dir := m.deps.MgmtLaunch.Dir
	rec, err := readMgmtLaunch(dir, instance.ID)
	if err != nil {
		return nil, err
	}
	if err := m.mgmtRecordMatches(rec, instance); err != nil {
		return nil, err
	}
	if err := m.mgmtProvenStopped(rec); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errMgmtLaunchUnresolved, instance.ID, err)
	}
	if m.deps.MgmtLaunch.Addresses == nil {
		return nil, errors.New("management address record not wired")
	}
	claim := m.deps.MgmtLaunch.Addresses.Confirm
	if fresh {
		claim = m.deps.MgmtLaunch.Addresses.Claim
	}
	if err := claim(instance.ID, instance.MgmtIP); err != nil {
		return nil, err
	}
	boot, err := mgmtBootID()
	if err != nil {
		return nil, fmt.Errorf("read boot id: %w", err)
	}
	next := *rec
	if rec.State != MgmtLaunchClaimed || rec.BootID != boot {
		next.Generation++
	}
	next.BootID, next.State, next.PID, next.StartTime = boot, MgmtLaunchStarting, 0, 0
	next.ReleasePending = false
	if err := writeMgmtLaunch(dir, &next); err != nil {
		return nil, fmt.Errorf("record launch of %s: %w", instance.ID, err)
	}
	instance.mgmtGen.Store(next.Generation)
	return &next, nil
}

// finishMgmtLaunch records what a started launch became. A start refused
// before cmd.Start is exited; otherwise the process identity is recorded. A
// failed write leaves the record starting, which holds the address.
func (m *Manager) finishMgmtLaunch(instance *VM, rec *mgmtLaunchRecord, startErr error) {
	next := *rec
	if errors.Is(startErr, errQEMUNotStarted) {
		next.State = MgmtLaunchExited
	} else {
		pid := int(instance.qemuPID.Load())
		if pid <= 0 {
			slog.Warn("Management launch has no process identity; holding its address",
				"instanceId", instance.ID, "generation", rec.Generation)
			return
		}
		next.State, next.PID = MgmtLaunchRunning, pid
		if _, st, err := procStat(mgmtProcRoot, pid); err == nil {
			next.StartTime = st
		}
	}
	if err := writeMgmtLaunch(m.deps.MgmtLaunch.Dir, &next); err != nil {
		slog.Error("Failed to record management launch outcome; holding its address",
			"instanceId", instance.ID, "generation", rec.Generation, "err", err)
	}
}

// mgmtGeneration is the launch a teardown of this object belongs to, read
// before the teardown can wait on the guard.
func (m *Manager) mgmtGeneration(instance *VM) uint64 {
	return instance.mgmtGen.Load()
}

// releaseMgmt gives back the address of the launch a teardown captured, only
// once that launch is proven stopped and is still the latest. Anything else
// keeps the reservation and says why. It reports false when the release was
// owed but could not be recorded, so the caller must keep the obligation.
func (m *Manager) releaseMgmt(instance *VM, gen uint64) bool {
	unlock := m.lockMgmt(instance)
	defer unlock()
	return m.releaseMgmtLocked(instance, gen)
}

// releaseMgmtLocked is releaseMgmt for a caller holding the guard.
func (m *Manager) releaseMgmtLocked(instance *VM, gen uint64) bool {
	if !m.mgmtDurable() || instance.MgmtIP == "" {
		return true
	}
	hold := func(reason string, err error) {
		slog.Warn("Holding management address", "instanceId", instance.ID, "ip", instance.MgmtIP,
			"generation", gen, "reason", reason, "err", err)
	}
	dir := m.deps.MgmtLaunch.Dir
	rec, err := readMgmtLaunch(dir, instance.ID)
	if err != nil {
		hold("launch record unreadable", err)
		return errors.Is(err, errMgmtLaunchMissing)
	}
	if err := m.mgmtRecordMatches(rec, instance); err != nil {
		hold("launch record names another node or address", err)
		return true
	}
	if rec.Generation != gen {
		hold("teardown belongs to an earlier launch", fmt.Errorf("record is at generation %d", rec.Generation))
		return true
	}
	// Recorded before the proof, so a launch that is not yet proven stopped is
	// still released by the retry once it is.
	if !rec.ReleasePending {
		marked := *rec
		marked.ReleasePending = true
		if err := writeMgmtLaunch(dir, &marked); err != nil {
			hold("release intent not recorded", err)
			return false
		}
		rec = &marked
	}
	if err := m.mgmtProvenStopped(rec); err != nil {
		hold("launch not proven stopped", err)
		return true
	}
	if m.deps.MgmtLaunch.Addresses == nil {
		hold("management address record not wired", nil)
		return true
	}
	m.completeMgmtRelease(rec)
	return true
}

// recordMgmtReleaseIntent marks a terminated instance's launch for release
// when its terminate could not. The release still waits for the retry and the
// launch record's own proof of stop.
func (m *Manager) recordMgmtReleaseIntent(v *VM) error {
	if !m.mgmtDurable() || v.MgmtIP == "" {
		return nil
	}
	unlock := m.mgmtLocks.lock(v.ID)
	defer unlock()
	rec, err := readMgmtLaunch(m.deps.MgmtLaunch.Dir, v.ID)
	switch {
	case errors.Is(err, errMgmtLaunchMissing):
		return nil
	case err != nil:
		return err
	case m.mgmtRecordMatches(rec, v) != nil || rec.ReleasePending:
		return nil
	}
	next := *rec
	next.ReleasePending = true
	return writeMgmtLaunch(m.deps.MgmtLaunch.Dir, &next)
}

// completeMgmtRelease gives back a proven-stopped launch's address and records
// the launch exited. An unacknowledged release leaves any pending mark for the
// retry. Called with the guard held, after the caller's proofs.
func (m *Manager) completeMgmtRelease(rec *mgmtLaunchRecord) bool {
	if err := m.deps.MgmtLaunch.Addresses.Release(rec.InstanceID, rec.MgmtIP); err != nil {
		slog.Warn("Holding management address", "instanceId", rec.InstanceID, "ip", rec.MgmtIP,
			"generation", rec.Generation, "reason", "release not acknowledged", "pending", rec.ReleasePending, "err", err)
		return false
	}
	if rec.State != MgmtLaunchExited || rec.ReleasePending {
		next := *rec
		next.State, next.ReleasePending = MgmtLaunchExited, false
		if err := writeMgmtLaunch(m.deps.MgmtLaunch.Dir, &next); err != nil {
			slog.Error("Failed to record management launch exit", "instanceId", rec.InstanceID, "err", err)
		}
	}
	return true
}

// mgmtTeardown runs a teardown's process, volume and network steps under the
// instance's guard, only while the launch it captured is still the latest. A
// newer launch, or a record that cannot be read, is left alone.
func (m *Manager) mgmtTeardown(instance *VM, gen uint64, step string, teardown func()) bool {
	unlock := m.lockMgmt(instance)
	defer unlock()
	if err := m.mgmtCurrentLocked(instance, gen); err != nil {
		level := slog.LevelError
		if errors.Is(err, errMgmtLaunchStale) {
			level = slog.LevelWarn
		}
		slog.Log(context.Background(), level, "Skipping teardown", "instanceId", instance.ID, "step", step, "err", err)
		return false
	}
	teardown()
	return true
}

// mgmtCurrentLocked reports whether gen is the instance's latest launch, so
// the caller may act on the process. The caller holds the ID guard.
func (m *Manager) mgmtCurrentLocked(instance *VM, gen uint64) error {
	if !m.mgmtDurable() || instance.MgmtIP == "" {
		return nil
	}
	rec, err := readMgmtLaunch(m.deps.MgmtLaunch.Dir, instance.ID)
	switch {
	case errors.Is(err, errMgmtLaunchMissing) && gen == 0:
		// No launch identity exists, so none can be newer; release still holds.
		return nil
	case err != nil:
		return fmt.Errorf("read launch record: %w", err)
	case m.mgmtRecordMatches(rec, instance) != nil:
		return m.mgmtRecordMatches(rec, instance)
	case rec.Generation != gen:
		return fmt.Errorf("%w: generation %d, latest %d", errMgmtLaunchStale, gen, rec.Generation)
	}
	return nil
}
