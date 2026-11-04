//go:build lambdatestlink

package testlink

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeQEMUExit is the stub's exit status, so a test that execs it sees a failed VM, never a launch.
const fakeQEMUExit = 97

// TestMain puts a stub qemu-system-x86_64 at the front of PATH for the whole run, so the VM command
// builds on hosts without QEMU; hostos caches the resolved path, so the stub outlives every test.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "testlink-fake-qemu-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake qemu dir:", err)
		os.Exit(1)
	}
	stub := fmt.Sprintf("#!/bin/sh\necho 'fake qemu-system-x86_64: not a VM' >&2\nexit %d\n", fakeQEMUExit)
	if err := os.WriteFile(filepath.Join(dir, "qemu-system-x86_64"), []byte(stub), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "fake qemu stub:", err)
		os.Exit(1)
	}
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		fmt.Fprintln(os.Stderr, "fake qemu PATH:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
