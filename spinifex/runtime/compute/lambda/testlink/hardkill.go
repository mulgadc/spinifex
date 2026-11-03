//go:build lambdatestlink

package testlink

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// HardKillMargin is how far past the attempt's own bound the in-QEMU timer
// fires; it backstops a daemon that stops enforcing the deadline.
const HardKillMargin = 10 * time.Second

// armWait bounds how long the daemon waits for the initializer's ack.
const armWait = 5 * time.Second

// ErrNotArmed means QEMU gave no valid proof that its hard-kill timer is armed.
var ErrNotArmed = errors.New("hard-kill timer not armed")

// checkInitializer refuses a missing initializer or one whose bytes differ
// from the pinned digest. ld.so loads by path later, so a file replaced after
// this check is not caught: this is not a production trust boundary.
func (d *Daemon) checkInitializer() error {
	if d.Initializer == "" || d.InitializerSHA256 == "" {
		return errors.New("hard-kill initializer not configured")
	}
	b, err := os.ReadFile(d.Initializer)
	if err != nil {
		return errors.New("hard-kill initializer unreadable")
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != d.InitializerSHA256 {
		return errors.New("hard-kill initializer digest mismatch")
	}
	return nil
}

// hardKill is one attempt's arming request and the pipe its ack arrives on.
type hardKill struct {
	ms    int64
	nonce string
	r, w  *os.File
}

// armCommand sets cmd's environment to the daemon's without any inherited
// dynamic-loader or backstop setting, then adds only this initializer, and
// passes the ack pipe as an extra file.
func armCommand(cmd *exec.Cmd, initializer string, deadline time.Duration) (*hardKill, error) {
	nb := make([]byte, 16)
	if _, err := rand.Read(nb); err != nil {
		return nil, fmt.Errorf("hard-kill nonce: %w", err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("hard-kill pipe: %w", err)
	}
	hk := &hardKill{ms: deadline.Milliseconds(), nonce: hex.EncodeToString(nb), r: r, w: w}
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
	fd := 3 + len(cmd.ExtraFiles)
	cmd.Env = append(env,
		"LD_PRELOAD="+initializer,
		"SPX_HARDKILL_MS="+strconv.FormatInt(hk.ms, 10),
		"SPX_HARDKILL_ACK_FD="+strconv.Itoa(fd),
		"SPX_HARDKILL_NONCE="+hk.nonce)
	cmd.ExtraFiles = append(cmd.ExtraFiles, w)
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
	want := fmt.Sprintf("armed %d %d %s\n", pid, hk.ms, hk.nonce)
	got := make([]byte, 0, len(want))
	buf := make([]byte, len(want)+1)
	for len(got) <= len(want) {
		n, err := hk.r.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) && bytes.Equal(got, []byte(want)) {
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
	return nil
}
