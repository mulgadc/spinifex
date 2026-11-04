//go:build lambdatestlink

package testlink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
)

// pidfsMagic is PID_FS_MAGIC, the filesystem a pidfd lives on when the kernel
// gives each process its own pidfd inode.
const pidfsMagic = 0x50494446

// recoverKillWait bounds how long recovery waits for a killed VM to exit.
const recoverKillWait = 10 * time.Second

var (
	// ErrNoPidfs: pidfds here are not on pidfs, so their inodes identify nothing.
	ErrNoPidfs = errors.New("lambda test link: pidfds are not on pidfs")
	// ErrLaunchNotRecorded: the owner did not confirm the launch identity.
	ErrLaunchNotRecorded = errors.New("lambda test link: launch identity not recorded")
	// ErrVMGone: the VM exited before its launch identity was taken.
	ErrVMGone = errors.New("lambda test link: VM exited before its launch was recorded")
)

// pidfsInode returns the pidfs inode of pidfd. Older anon-inode pidfds all
// share one inode, so a pidfd on any other filesystem is refused.
func pidfsInode(pidfd int) (uint64, error) {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(pidfd, &fs); err != nil {
		return 0, fmt.Errorf("fstatfs pidfd: %w", err)
	}
	if fs.Type != pidfsMagic {
		return 0, ErrNoPidfs
	}
	var st unix.Stat_t
	if err := unix.Fstat(pidfd, &st); err != nil {
		return 0, fmt.Errorf("fstat pidfd: %w", err)
	}
	if st.Ino == 0 {
		return 0, ErrNoPidfs
	}
	return st.Ino, nil
}

// selfInode returns this process's pidfs inode.
func selfInode() (uint64, error) {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return 0, fmt.Errorf("pidfd for this process: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	return pidfsInode(fd)
}

// hostIDs is what scopes a recorded PID and pidfs inode.
type hostIDs struct {
	machine, boot string
	pidns         uint64
}

func readHost() (hostIDs, error) {
	m, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return hostIDs{}, fmt.Errorf("read machine ID: %w", err)
	}
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return hostIDs{}, fmt.Errorf("read boot ID: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &st); err != nil {
		return hostIDs{}, fmt.Errorf("read PID namespace: %w", err)
	}
	h := hostIDs{machine: strings.TrimSpace(string(m)), boot: strings.TrimSpace(string(b)), pidns: st.Ino}
	if h.machine == "" || h.boot == "" || h.pidns == 0 {
		return hostIDs{}, errors.New("host identity incomplete")
	}
	return h, nil
}

// launchIdentity identifies the VM by the exact pidfd taken when it was cloned,
// so a reaped and reused PID can never stand in for it.
func launchIdentity(pidfd, pid int, tap host.OwnedTap) (execution.Launch, error) {
	ino, err := pidfsInode(pidfd)
	if err != nil {
		return execution.Launch{}, err
	}
	if exited, err := pidfdExited(pidfd, 0); err != nil {
		return execution.Launch{}, err
	} else if exited {
		return execution.Launch{}, ErrVMGone
	}
	self, err := selfInode()
	if err != nil {
		return execution.Launch{}, err
	}
	h, err := readHost()
	if err != nil {
		return execution.Launch{}, err
	}
	return execution.Launch{MachineID: h.machine, BootID: h.boot, PIDNamespace: h.pidns,
		LauncherPID: os.Getpid(), LauncherInode: self, PID: pid, Inode: ino,
		TapName: tap.Name, TapIndex: tap.IfIndex, TapOwner: tap.Owner}, nil
}

// pidfdExited reports whether pidfd's process has exited, waiting up to wait.
func pidfdExited(pidfd int, wait time.Duration) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, int(wait.Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("poll pidfd: %w", err)
		}
		if n == 0 {
			return false, nil
		}
		// Only readability is the kernel's exit report; anything else proves nothing.
		ev := fds[0].Revents
		if ev&(unix.POLLNVAL|unix.POLLERR) != 0 || ev&unix.POLLIN == 0 {
			return false, fmt.Errorf("pidfd poll returned unexpected events %#x", ev)
		}
		return true, nil
	}
}

// processGone reports whether the process recorded as pid with pidfs inode ino
// no longer exists. With kill set, a live match is killed through its own pidfd
// and reported gone only once that pidfd shows the exit.
func processGone(pid int, ino uint64, kill bool) (bool, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("pidfd for %d: %w", pid, err)
	}
	defer func() { _ = unix.Close(fd) }()
	got, err := pidfsInode(fd)
	if err != nil {
		return false, err
	}
	if got != ino {
		// The PID names another live process, so the recorded one is gone.
		return true, nil
	}
	if !kill {
		return false, nil
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return false, fmt.Errorf("kill recorded VM: %w", err)
	}
	return pidfdExited(fd, recoverKillWait)
}

// proveStopped proves the recorded VM is gone. A recorded PID and inode mean
// something only on the same boot and PID namespace with no launcher left, so
// anything else, a reboot included, is held.
func proveStopped(l execution.Launch) error {
	h, err := readHost()
	if err != nil {
		return err
	}
	if h.boot != l.BootID || h.machine != l.MachineID {
		return errors.New("launch recorded on another boot or host")
	}
	if h.pidns != l.PIDNamespace {
		return errors.New("launch recorded in another PID namespace")
	}
	if gone, err := processGone(l.LauncherPID, l.LauncherInode, false); err != nil {
		return fmt.Errorf("launcher: %w", err)
	} else if !gone {
		return errors.New("recorded launcher still runs")
	}
	gone, err := processGone(l.PID, l.Inode, true)
	if err != nil {
		return fmt.Errorf("VM: %w", err)
	}
	if !gone {
		return errors.New("recorded VM did not exit after kill")
	}
	return nil
}

// RecoverFirst is the first retry interval; it doubles up to RecoverMax.
const (
	RecoverFirst = 5 * time.Second
	RecoverMax   = 5 * time.Minute
)

// recoverWait sleeps between rounds; it reports false once ctx ends.
var recoverWait = func(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RecoverLoop retries Recover on ctx until it settles, waiting first and then
// doubling up to ceiling. Each round takes the node's slot, so it never runs
// beside Handle. It covers what a previous daemon left, not a later Handle.
func (d *Daemon) RecoverLoop(ctx context.Context, first, ceiling time.Duration) {
	wait := first
	for {
		if d.busy.CompareAndSwap(false, true) {
			settled := d.Recover(ctx)
			d.busy.Store(false)
			if settled {
				return
			}
		}
		if !recoverWait(ctx, wait) {
			return
		}
		wait = min(wait*2, ceiling)
	}
}

// Recover proves the stop of an attempt an earlier daemon on this boot left
// unended, then releases what it held. Anything it cannot prove stays held,
// and the node with it. It reports whether nothing is left to recover.
func (d *Daemon) Recover(ctx context.Context) bool {
	cur, err := d.Grants.Current(ctx, d.Node)
	if errors.Is(err, execution.ErrNotCurrent) {
		return true
	}
	if err != nil {
		slog.Error("Lambda test link: recovery cannot read the node record; node held", "err", err)
		return false
	}
	id := cur.Grant.AttemptID
	if !attemptIDPattern.MatchString(id) {
		slog.Error("Lambda test link: recovery found an unrecognised attempt ID; node held")
		return false
	}
	marker := filepath.Join(d.WorkDir, id+markerSuffix)
	switch cur.State {
	case execution.StateRunning, execution.StateAccepted, execution.StateAmbiguous:
	case execution.StateReleased:
		if !d.complete(ctx, cur.Grant, marker) {
			return false
		}
		slog.Info("Lambda test link: recovered attempt stopped", "attempt", id)
		return true
	default:
		return true
	}
	if cur.Launch == nil {
		slog.Error("Lambda test link: attempt has no launch identity; held", "attempt", id)
		return false
	}
	l := *cur.Launch
	if err := proveStopped(l); err != nil {
		slog.Error("Lambda test link: VM stop not proven at recovery; held", "attempt", id, "err", err)
		return false
	}
	a := &held{id: "lambda-" + id, dir: filepath.Join(d.WorkDir, id), marker: marker, ip: true,
		tap: host.OwnedTap{Name: l.TapName, IfIndex: l.TapIndex, Owner: l.TapOwner}}
	if !d.release(a) || !d.finish(ctx, cur.Grant, true, marker) {
		return false
	}
	slog.Info("Lambda test link: recovered attempt stopped", "attempt", id)
	return true
}
