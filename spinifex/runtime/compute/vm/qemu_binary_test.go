package vm

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/host/hostos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	resolveQEMUBinary = fakeQEMUBinary
	os.Exit(m.Run())
}

// fakeQEMUBinary stands in for hostos.QEMUBinary so Execute tests need no QEMU
// installed on the host.
func fakeQEMUBinary(arch string) (string, error) {
	switch arch {
	case "x86_64":
		return "/usr/bin/qemu-system-x86_64", nil
	case "arm64":
		return "/usr/bin/qemu-system-aarch64", nil
	}
	return "", fmt.Errorf("no QEMU for guest architecture %q", arch)
}

func withQEMUResolver(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	orig := resolveQEMUBinary
	resolveQEMUBinary = fn
	t.Cleanup(func() { resolveQEMUBinary = orig })
}

func minimalConfig() Config {
	return Config{
		CPUCount:     1,
		Memory:       512,
		Architecture: "x86_64",
		Drives:       []Drive{{File: "disk.img", Format: "raw"}},
	}
}

func TestExecute_RunsTheResolvedQEMUBinary(t *testing.T) {
	var asked string
	withQEMUResolver(t, func(arch string) (string, error) {
		asked = arch
		return hostos.QEMUKVMPath, nil
	})

	cfg := minimalConfig()
	cmd, err := cfg.Execute()
	require.NoError(t, err)
	assert.Equal(t, "x86_64", asked)
	assert.Equal(t, hostos.QEMUKVMPath, cmd.Path)
	assert.Equal(t, hostos.QEMUKVMPath, cmd.Args[0])
}

func TestExecute_FailsWhenNoQEMUResolves(t *testing.T) {
	noQEMU := errors.New("no QEMU for x86_64 guests: found neither /usr/libexec/qemu-kvm (package qemu-kvm) nor qemu-system-x86_64 (package qemu-system-x86) on PATH")
	withQEMUResolver(t, func(string) (string, error) { return "", noQEMU })

	cfg := minimalConfig()
	cmd, err := cfg.Execute()
	assert.Nil(t, cmd)
	assert.ErrorIs(t, err, noQEMU)
}
