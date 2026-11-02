package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDetectHostVirtStack names the binary, its version line and the exact
// firmware build, which is what a boot hang report has to carry to be
// reproducible on another host.
func TestDetectHostVirtStack(t *testing.T) {
	dir := t.TempDir()
	qemu := filepath.Join(dir, "qemu-system-x86_64")
	require.NoError(t, os.WriteFile(qemu,
		[]byte("#!/bin/sh\necho 'QEMU emulator version 10.0.13 (Debian 1:10.0.13+ds-0+deb13u1)'\necho 'Copyright'\n"), 0o755))
	withQEMUResolver(t, func(string) (string, error) { return qemu, nil })

	code := filepath.Join(dir, "OVMF_CODE.fd")
	vars := filepath.Join(dir, "OVMF_VARS.fd")
	require.NoError(t, os.WriteFile(code, []byte("firmware"), 0o600))
	require.NoError(t, os.WriteFile(vars, []byte("vars"), 0o600))
	prev := FirmwarePathCandidates
	FirmwarePathCandidates = map[string][]FirmwareCandidate{"x86_64": {{Code: code, VarsTemplate: vars}}}
	t.Cleanup(func() { FirmwarePathCandidates = prev })

	s := detectHostVirtStack("x86_64")

	sum := sha256.Sum256([]byte("firmware"))
	assert.Equal(t, qemu, s.QEMU)
	assert.Equal(t, "QEMU emulator version 10.0.13 (Debian 1:10.0.13+ds-0+deb13u1)", s.QEMUVersion)
	assert.Equal(t, code, s.Firmware)
	assert.Equal(t, hex.EncodeToString(sum[:]), s.FirmwareSHA256)
	assert.Empty(t, s.FirmwarePackage, "a file no package installed has no owner to name")
	assert.NotEmpty(t, s.Kernel)
}

// TestPackageOwning_DistroFirmware resolves the package of a file the host's
// package manager does own, when there is one to ask.
func TestPackageOwning_DistroFirmware(t *testing.T) {
	const path = "/bin/sh"
	if runHostProbe("dpkg-query", "-S", path) == "" && runHostProbe("rpm", "-qf", path) == "" {
		t.Skip("no package manager owns /bin/sh on this host")
	}
	assert.NotEmpty(t, packageOwning(path))
}
