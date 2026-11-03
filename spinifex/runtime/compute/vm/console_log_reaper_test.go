package vm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// consoleBody returns n bytes of 16-byte numbered lines, so any slice of it
// identifies which part of the original it came from.
func consoleBody(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "l%014d\n", i)
	}
	return b.Bytes()[:n]
}

func newConsoleReaper(t *testing.T, now time.Time) (*ConsoleLogReaper, string) {
	t.Helper()
	dir := t.TempDir()
	r := NewConsoleLogReaper(dir)
	r.now = func() time.Time { return now }
	return r, dir
}

func writeConsole(t *testing.T, dir, id string, body []byte, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(dir, "console-"+id+".log")
	require.NoError(t, os.WriteFile(p, body, 0o644))
	require.NoError(t, os.Chtimes(p, mtime, mtime))
	return p
}

func TestConsoleLogReaper_TrimsToNewestMiB(t *testing.T) {
	now := time.Now()
	r, dir := newConsoleReaper(t, now)
	body := consoleBody(5 << 20)
	p := writeConsole(t, dir, "i-big", body, now)

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)

	got, err := os.ReadFile(p)
	require.NoError(t, err)
	gotSize := len(got)
	require.Equal(t, consoleKeepBytes, gotSize, "a trim keeps exactly the newest MiB")
	assert.True(t, bytes.Equal(body[len(body)-consoleKeepBytes:], got), "the newest MiB must survive, in order")
}

func TestConsoleLogReaper_LeavesFilesAtOrBelowTheLimit(t *testing.T) {
	now := time.Now()
	r, dir := newConsoleReaper(t, now)
	small := consoleBody(3 << 20)
	limit := consoleBody(consoleTrimAbove)
	ps := writeConsole(t, dir, "i-small", small, now)
	pl := writeConsole(t, dir, "i-limit", limit, now)

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Zero(t, reaped)

	for p, want := range map[string][]byte{ps: small, pl: limit} {
		got, err := os.ReadFile(p)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(want, got), "%s must not change", filepath.Base(p))
	}
}

// QEMU writes through a descriptor it keeps open, so the trim must leave the
// file usable for that writer: no hole of NULs, and later output at the end.
func TestConsoleLogReaper_AppendWriterKeepsWritingAfterTrim(t *testing.T) {
	now := time.Now()
	r, dir := newConsoleReaper(t, now)
	p := filepath.Join(dir, "console-i-live.log")
	w, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	require.NoError(t, err)
	defer w.Close()
	_, err = w.Write(consoleBody(5 << 20))
	require.NoError(t, err)

	_, err = r.Sweep(context.Background())
	require.NoError(t, err)

	marker := []byte("after-trim-marker\n")
	_, err = w.Write(marker)
	require.NoError(t, err)

	got, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Zero(t, bytes.Count(got, []byte{0}), "no NUL hole from a stale write offset")
	assert.True(t, bytes.HasSuffix(got, marker), "the writer's next line lands at the end")
	assert.LessOrEqual(t, len(got), consoleKeepBytes+len(marker))
}

func TestConsoleLogReaper_RemovesOnlyAgedRetiredFiles(t *testing.T) {
	now := time.Now()
	r, dir := newConsoleReaper(t, now)
	old := now.Add(-consoleRemoveAfter - time.Minute)
	recent := now.Add(-consoleRemoveAfter + time.Minute)
	retiredOld := writeConsole(t, dir, "i-gone-old", []byte("x\n"), old)
	retiredNew := writeConsole(t, dir, "i-gone-new", []byte("x\n"), recent)
	require.NoError(t, os.Rename(retiredOld, RetiredConsolePath(retiredOld)))
	require.NoError(t, os.Rename(retiredNew, RetiredConsolePath(retiredNew)))
	require.NoError(t, os.Chtimes(RetiredConsolePath(retiredOld), old, old))
	require.NoError(t, os.Chtimes(RetiredConsolePath(retiredNew), recent, recent))
	// A stopped or running instance's file, however old, is never removed.
	stopped := writeConsole(t, dir, "i-stopped", []byte("boot\n"), now.Add(-720*time.Hour))
	other := filepath.Join(dir, "serial-i-x.sock.log.terminated")
	require.NoError(t, os.WriteFile(other, []byte("x"), 0o644))
	require.NoError(t, os.Chtimes(other, old, old))

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped, "only the retired file older than an hour")

	_, err = os.Stat(RetiredConsolePath(retiredOld))
	assert.ErrorIs(t, err, os.ErrNotExist, "retired over an hour ago: removed")
	_, err = os.Stat(RetiredConsolePath(retiredNew))
	assert.NoError(t, err, "retired recently: kept for a late GetConsoleOutput")
	_, err = os.Stat(stopped)
	assert.NoError(t, err, "console-<id>.log is never removed whatever its age, AWS returns it after stop")
	_, err = os.Stat(other)
	assert.NoError(t, err, "files that are not console logs are never touched")
}

func TestRetireConsoleLog(t *testing.T) {
	dir := t.TempDir()
	p := writeConsole(t, dir, "i-end", []byte("last words\n"), time.Now())

	retireConsoleLog(p)
	_, err := os.Stat(p)
	assert.ErrorIs(t, err, os.ErrNotExist, "the live name no longer matches the shipper's glob")
	got, err := os.ReadFile(RetiredConsolePath(p))
	require.NoError(t, err)
	assert.Equal(t, "last words\n", string(got))

	assert.NotPanics(t, func() { retireConsoleLog(p) }, "a repeat finalize is a no-op")
	assert.NotPanics(t, func() { retireConsoleLog("") }, "an instance that never got a console path is a no-op")
}

func TestConsoleLogReaper_TrimsOnlyLiveFiles(t *testing.T) {
	now := time.Now()
	r, dir := newConsoleReaper(t, now)
	big := writeConsole(t, dir, "i-big", consoleBody(5<<20), now)
	retired := writeConsole(t, dir, "i-gone", consoleBody(5<<20), now)
	require.NoError(t, os.Rename(retired, RetiredConsolePath(retired)))

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)
	info, err := os.Stat(big)
	require.NoError(t, err)
	assert.Equal(t, int64(consoleKeepBytes), info.Size())
	info, err = os.Stat(RetiredConsolePath(retired))
	require.NoError(t, err)
	assert.Equal(t, int64(5<<20), info.Size(), "a retired file is final and is only aged out")
}

func TestExecute_ConsoleAlwaysAppends(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"socket first boot", Config{SerialSocket: "/s", ConsoleLogPath: "/c"}, "logfile=/c,logappend=on"},
		{"microvm first boot", Config{MachineType: "microvm", ConsoleLogPath: "/c"}, "file,id=console0,path=/c,append=on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalConfig()
			cfg.MachineType, cfg.SerialSocket = tc.cfg.MachineType, tc.cfg.SerialSocket
			cfg.ConsoleLogPath = tc.cfg.ConsoleLogPath
			cmd, err := cfg.Execute()
			require.NoError(t, err)
			var chardev string
			for i, a := range cmd.Args {
				if a == "-chardev" && i+1 < len(cmd.Args) {
					chardev = cmd.Args[i+1]
				}
			}
			assert.Contains(t, chardev, tc.want, "a truncating open would let an in-place trim leave a NUL hole")
		})
	}
}
