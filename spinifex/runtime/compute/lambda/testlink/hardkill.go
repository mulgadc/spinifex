//go:build lambdatestlink

package testlink

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// HardKillMargin is how far past the attempt's own bound the in-QEMU timer
// fires; it backstops a daemon that stops enforcing the deadline.
const HardKillMargin = 10 * time.Second

// armWait bounds how long the daemon waits for the initializer's ack.
const armWait = 5 * time.Second

// ErrNotArmed means QEMU gave no valid proof that its hard-kill timer is armed.
var ErrNotArmed = errors.New("hard-kill timer not armed")

// ErrDeadlineExpired means the attempt's fixed hard-kill deadline has passed,
// so nothing may be dispatched to the guest.
var ErrDeadlineExpired = errors.New("hard-kill deadline expired")

// bootNow reads CLOCK_BOOTTIME, which keeps counting through suspend; a var so
// tests can move it.
var bootNow = func() (time.Duration, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, fmt.Errorf("read CLOCK_BOOTTIME: %w", err)
	}
	return time.Duration(ts.Nano()), nil
}

// expired reports whether the absolute boottime deadline has passed; an
// unreadable clock counts as expired.
func expired(deadline time.Duration) bool {
	t, err := bootNow()
	return err != nil || t >= deadline
}

// maxInitializer bounds the bytes read for the hard-kill initializer.
const maxInitializer = 1 << 20

// requiredSeals stop any later write, resize or seal change to the memfd.
const requiredSeals = unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE

// sealInitializer reads the initializer once, checks those bytes against the
// pinned digest, and returns them in a sealed memfd that QEMU loads by fd. It
// authenticates nothing about where the pinned digest came from.
func (d *Daemon) sealInitializer() (*os.File, error) {
	if d.Initializer == "" || d.InitializerSHA256 == "" {
		return nil, errors.New("hard-kill initializer not configured")
	}
	f, err := os.Open(d.Initializer)
	if err != nil {
		return nil, errors.New("hard-kill initializer unreadable")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxInitializer+1))
	_ = f.Close()
	if err != nil || len(b) == 0 || len(b) > maxInitializer {
		return nil, errors.New("hard-kill initializer unreadable")
	}
	if !digestIs(b, d.InitializerSHA256) {
		return nil, errors.New("hard-kill initializer digest mismatch")
	}
	if err := sameArch(b); err != nil {
		return nil, err
	}
	m, err := sealedCopy(b)
	if err != nil {
		return nil, fmt.Errorf("hard-kill initializer seal: %w", err)
	}
	// The sealed object, not the buffer, is what QEMU maps: check it again.
	got := make([]byte, len(b)+1)
	n, err := m.ReadAt(got, 0)
	if (err != nil && !errors.Is(err, io.EOF)) || !digestIs(got[:n], d.InitializerSHA256) {
		_ = m.Close()
		return nil, errors.New("hard-kill initializer seal: sealed copy differs")
	}
	return m, nil
}

// CheckInitializer proves the configured initializer matches the pin and this
// daemon's architecture, so a mismatched pair is refused before serving.
func (d *Daemon) CheckInitializer() error {
	m, err := d.sealInitializer()
	if err != nil {
		return err
	}
	return m.Close()
}

// elfMachine maps GOARCH to the ELF machine an initializer must be built for.
var elfMachine = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

// sameArch refuses an initializer whose ELF machine differs from the daemon's.
func sameArch(b []byte) error {
	want, ok := elfMachine[runtime.GOARCH]
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return errors.New("hard-kill initializer is not an ELF object")
	}
	if !ok || f.Machine != want || f.Class != elf.ELFCLASS64 {
		return fmt.Errorf("hard-kill initializer built for %v, daemon is %s", f.Machine, runtime.GOARCH)
	}
	return nil
}

// memfdCreate is unix.MemfdCreate; tests replace it to reach kernel refusals.
var memfdCreate = unix.MemfdCreate

func digestIs(b []byte, want string) bool {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]) == want
}

// sealedCopy writes b to a new memfd and applies requiredSeals, failing
// unless the kernel reports exactly those seals afterwards.
func sealedCopy(b []byte) (*os.File, error) {
	fd, err := memfdCreate("spx-hardkill", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_EXEC)
	switch {
	case errors.Is(err, unix.EINVAL):
		// An old kernel or a security filter: no retry without MFD_EXEC.
		return nil, fmt.Errorf("MFD_EXEC unavailable or refused: %w", err)
	case errors.Is(err, unix.EACCES):
		// Possibly vm.memfd_noexec, possibly another policy: never bypassed.
		return nil, fmt.Errorf("executable memfd refused: %w", err)
	case err != nil:
		return nil, err
	}
	m := os.NewFile(uintptr(fd), "spx-hardkill")
	if _, err := m.Write(b); err != nil {
		_ = m.Close()
		return nil, err
	}
	if _, err := unix.FcntlInt(m.Fd(), unix.F_ADD_SEALS, requiredSeals); err != nil {
		_ = m.Close()
		return nil, err
	}
	seals, err := unix.FcntlInt(m.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals&requiredSeals != requiredSeals {
		_ = m.Close()
		return nil, errors.New("seals not in place")
	}
	return m, nil
}

// hardKill is one attempt's arming request and the pipe its ack arrives on.
type hardKill struct {
	deadline time.Duration // absolute CLOCK_BOOTTIME
	nonce    string
	r, w     *os.File
}

// armCommand sets cmd's environment to the daemon's without any inherited
// dynamic-loader or backstop setting, then preloads lib by its inherited fd
// and passes the ack pipe and the absolute boottime deadline; the caller still owns lib.
func armCommand(cmd *exec.Cmd, lib *os.File, deadline time.Duration) (*hardKill, error) {
	if deadline <= 0 {
		return nil, errors.New("hard-kill deadline not set")
	}
	nb := make([]byte, 16)
	if _, err := rand.Read(nb); err != nil {
		return nil, fmt.Errorf("hard-kill nonce: %w", err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("hard-kill pipe: %w", err)
	}
	hk := &hardKill{deadline: deadline, nonce: hex.EncodeToString(nb), r: r, w: w}
	base := cmd.Env
	if base == nil {
		base = os.Environ()
	}
	env := make([]string, 0, len(base)+4)
	for _, kv := range base {
		if strings.HasPrefix(kv, "LD_") || strings.HasPrefix(kv, "SPX_HARDKILL_") {
			continue
		}
		env = append(env, kv)
	}
	ack := 3 + len(cmd.ExtraFiles)
	cmd.Env = append(env,
		"LD_PRELOAD=/proc/self/fd/"+strconv.Itoa(ack+1),
		"SPX_HARDKILL_LIB_FD="+strconv.Itoa(ack+1),
		"SPX_HARDKILL_DEADLINE_NS="+strconv.FormatInt(int64(hk.deadline), 10),
		"SPX_HARDKILL_ACK_FD="+strconv.Itoa(ack),
		"SPX_HARDKILL_NONCE="+hk.nonce)
	cmd.ExtraFiles = append(cmd.ExtraFiles, w, lib)
	return hk, nil
}

// close drops both pipe ends; safe to call more than once.
func (hk *hardKill) close() {
	_ = hk.w.Close()
	_ = hk.r.Close()
}

// await requires the exact ack for pid before anything is sent to the guest.
// The daemon's copy of the write end is closed first, so a QEMU that exits or
// never arms the timer ends the read instead of holding it open.
func (hk *hardKill) await(ctx context.Context, pid int, wait time.Duration) error {
	_ = hk.w.Close()
	deadline := now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := hk.r.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("%w: %w", ErrNotArmed, err)
	}
	want := fmt.Sprintf("armed %d %d %s\n", pid, int64(hk.deadline), hk.nonce)
	got := make([]byte, 0, len(want))
	buf := make([]byte, len(want)+1)
	for len(got) <= len(want) {
		n, err := hk.r.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) && bytes.Equal(got, []byte(want)) {
				if expired(hk.deadline) {
					return ErrDeadlineExpired
				}
				return nil
			}
			break
		}
		if bytes.HasSuffix(got, []byte("\n")) {
			break
		}
	}
	if !bytes.Equal(got, []byte(want)) {
		return ErrNotArmed
	}
	// An ack read at or after the deadline may have raced the due kill.
	if expired(hk.deadline) {
		return ErrDeadlineExpired
	}
	return nil
}
