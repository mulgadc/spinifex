package vm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
)

// FirmwareCandidate is a (CODE, VARS template) pair shipped together by a
// firmware package. CODE is mounted readonly as pflash unit 0; VARS seeds the
// instance's variable store, which is mounted as pflash unit 1 so EFI
// variables (BootOrder, BootNext, secure-boot state) survive reboots.
type FirmwareCandidate struct {
	Code         string
	VarsTemplate string
}

// FirmwarePathCandidates lists UEFI firmware locations keyed by architecture.
// First match wins so distro packages take precedence over edk2 fallbacks.
// Exported so tests can swap entries via t.Cleanup.
var FirmwarePathCandidates = map[string][]FirmwareCandidate{
	"x86_64": {
		{Code: "/usr/share/OVMF/OVMF_CODE_4M.fd", VarsTemplate: "/usr/share/OVMF/OVMF_VARS_4M.fd"},
		{Code: "/usr/share/edk2/ovmf/OVMF_CODE.fd", VarsTemplate: "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
	},
	"arm64": {
		{Code: "/usr/share/AAVMF/AAVMF_CODE.fd", VarsTemplate: "/usr/share/AAVMF/AAVMF_VARS.fd"},
		{Code: "/usr/share/edk2/aarch64/QEMU_EFI-pflash.raw", VarsTemplate: "/usr/share/edk2/aarch64/vars-template-pflash.raw"},
	},
}

// FirmwarePaths returns the first (code, varsTemplate, varsSize) from
// FirmwarePathCandidates[arch] where both files exist. varsSize is used to
// size the per-VM VARS volume. Returns an error on miss — no SeaBIOS fallback.
func FirmwarePaths(arch string) (code, varsTemplate string, varsSize int64, err error) {
	candidates, ok := FirmwarePathCandidates[arch]
	if !ok {
		return "", "", 0, fmt.Errorf("no UEFI firmware candidates registered for architecture %q", arch)
	}
	for _, c := range candidates {
		if _, statErr := os.Stat(c.Code); statErr != nil {
			continue
		}
		varsInfo, statErr := os.Stat(c.VarsTemplate)
		if statErr != nil {
			continue
		}
		return c.Code, c.VarsTemplate, varsInfo.Size(), nil
	}
	return "", "", 0, fmt.Errorf("no UEFI firmware found for architecture %q; install ovmf (x86_64) or qemu-efi-aarch64 (arm64)", arch)
}

// NodeFirmware is the UEFI firmware this node runs for one architecture: the
// candidate FirmwarePaths matched and the identity a variable store is
// checked against.
type NodeFirmware struct {
	Code         string
	VarsTemplate string
	Identity     uefistore.FirmwareIdentity
}

// codeHashKey identifies one version of a CODE blob on disk, so a package
// update that replaces the file is hashed again.
type codeHashKey struct {
	path    string
	size    int64
	modTime time.Time
}

// codeHashes caches CODE blob hashes by codeHashKey. The arm64 blob is 64 MiB,
// and every UEFI launch needs its hash.
var codeHashes sync.Map

// NodeFirmwareFor returns the firmware this node runs for arch, with the
// SHA-256 of its CODE blob.
func NodeFirmwareFor(arch string) (NodeFirmware, error) {
	code, vars, varsSize, err := FirmwarePaths(arch)
	if err != nil {
		return NodeFirmware{}, err
	}
	sum, err := codeSHA256(code)
	if err != nil {
		return NodeFirmware{}, err
	}
	return NodeFirmware{
		Code:         code,
		VarsTemplate: vars,
		Identity:     uefistore.FirmwareIdentity{Arch: arch, VarsSize: varsSize, CodeSHA256: sum},
	}, nil
}

func codeSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open UEFI CODE %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat UEFI CODE %s: %w", path, err)
	}
	key := codeHashKey{path: path, size: info.Size(), modTime: info.ModTime()}
	if cached, ok := codeHashes.Load(key); ok {
		if sum, ok := cached.(string); ok {
			return sum, nil
		}
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash UEFI CODE %s: %w", path, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	codeHashes.Store(key, sum)
	return sum, nil
}
