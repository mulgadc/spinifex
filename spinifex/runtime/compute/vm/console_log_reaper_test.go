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

func newConsoleReaper(t *testing.T, entries map[string]ConsoleLogEntry, now time.Time) (*ConsoleLogReaper, string) {
	t.Helper()
	dir := t.TempDir()
	r := NewConsoleLogReaper(dir, func() (map[string]ConsoleLogEntry, error) { return entries, nil })
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
	r, dir := newConsoleReaper(t, map[string]ConsoleLogEntry{"i-big": {State: StateRunning}}, now)
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
	r, dir := newConsoleReaper(t, map[string]ConsoleLogEntry{
		"i-small": {State: StateRunning},
		"i-limit": {State: StateRunning},
	}, now)
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
	r, dir := newConsoleReaper(t, map[string]ConsoleLogEntry{"i-live": {State: StateRunning}}, now)
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

func TestConsoleLogReaper_RemovesByInstanceState(t *testing.T) {
	now := time.Now()
	entries := map[string]ConsoleLogEntry{
		"i-term-old":     {State: StateTerminated, TerminatedAt: now.Add(-consoleRemoveAfter - time.Minute)},
		"i-term-new":     {State: StateTerminated, TerminatedAt: now.Add(-consoleRemoveAfter + time.Minute)},
		"i-term-nostamp": {State: StateTerminated},
		"i-running":      {State: StateRunning},
		"i-stopped":      {State: StateStopped},
	}
	r, dir := newConsoleReaper(t, entries, now)
	old := now.Add(-48 * time.Hour)
	paths := map[string]string{}
	for id := range entries {
		paths[id] = writeConsole(t, dir, id, []byte("boot\n"), old)
	}

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped, "only the instance terminated over an hour ago")

	exists := func(id string) bool { _, err := os.Stat(paths[id]); return err == nil }
	assert.False(t, exists("i-term-old"), "terminated over an hour ago: removed")
	assert.True(t, exists("i-term-new"), "terminated recently: kept for the shipper and late reads")
	assert.True(t, exists("i-term-nostamp"), "no terminated-at stamp: never guessed old")
	assert.True(t, exists("i-running"), "running: kept")
	assert.True(t, exists("i-stopped"), "stopped: kept, AWS returns console output after stop")
}

func TestConsoleLogReaper_UnknownFilesGoByMtime(t *testing.T) {
	now := time.Now()
	r, dir := newConsoleReaper(t, map[string]ConsoleLogEntry{}, now)
	fresh := writeConsole(t, dir, "i-unknown-fresh", []byte("boot\n"), now.Add(-consoleRemoveAfter+time.Minute))
	stale := writeConsole(t, dir, "i-unknown-stale", []byte("boot\n"), now.Add(-consoleRemoveAfter-time.Minute))
	other := filepath.Join(dir, "serial-i-unknown-stale.sock.log")
	require.NoError(t, os.WriteFile(other, []byte("x"), 0o644))
	require.NoError(t, os.Chtimes(other, now.Add(-72*time.Hour), now.Add(-72*time.Hour)))

	reaped, err := r.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)

	_, err = os.Stat(fresh)
	assert.NoError(t, err, "an unknown file with a fresh mtime may belong to an instance still loading")
	_, err = os.Stat(stale)
	assert.ErrorIs(t, err, os.ErrNotExist, "an unknown file untouched for over an hour is an orphan")
	_, err = os.Stat(other)
	assert.NoError(t, err, "files that are not console logs are never touched")
}

func TestConsoleLogReaper_ListFailureTouchesNothing(t *testing.T) {
	now := time.Now()
	r, dir := newConsoleReaper(t, nil, now)
	r.instances = func() (map[string]ConsoleLogEntry, error) { return nil, fmt.Errorf("records unavailable") }
	p := writeConsole(t, dir, "i-any", consoleBody(5<<20), now.Add(-72*time.Hour))

	_, err := r.Sweep(context.Background())
	require.Error(t, err)

	info, statErr := os.Stat(p)
	require.NoError(t, statErr, "an unreadable record set must not look like every instance is gone")
	assert.Equal(t, int64(5<<20), info.Size())
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
