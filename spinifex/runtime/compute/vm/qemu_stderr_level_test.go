// Tests the classification of QEMU's stderr lines, which carry a clean
// shutdown as well as faults.

package vm

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestQEMUStderrLevel(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		level slog.Level
	}{
		{
			// Verbatim from a production node, where the sending pid is spx
			// itself: the daemon stopping a guest it owns.
			name:  "the notice for the signal a stop sends",
			line:  "qemu-system-x86_64: terminating on signal 15 from pid 3464416 (/usr/local/bin/spx)",
			level: slog.LevelInfo,
		},
		{
			name:  "a device that could not be created",
			line:  "qemu-system-x86_64: -device virtio-blk-pci,drive=drive0: Device needs media, but drive is empty",
			level: slog.LevelError,
		},
		{
			name:  "a block backend that could not be opened",
			line:  "qemu-system-x86_64: -blockdev nbd: Failed to connect to '/var/lib/spinifex/nbd.sock': No such file or directory",
			level: slog.LevelError,
		},
		{
			// What a dev run gets on Ctrl-C, and a shutdown for the same
			// reason the SIGTERM notice is.
			name:  "the notice for an interrupt",
			line:  "qemu-system-x86_64: terminating on signal 2",
			level: slog.LevelInfo,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.level, qemuStderrLevel(tc.line))
		})
	}
}
