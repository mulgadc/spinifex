package vm

import (
	"fmt"
	"os"
)

// FirmwareCandidate is a (CODE, VARS template) pair shipped together by a
// firmware package. CODE is mounted readonly as pflash unit 0; VARS is copied
// into a per-VM viperblock volume on first boot and mounted as pflash unit 1
// so EFI variables (BootOrder, BootNext, secure-boot state) survive reboots.
type FirmwareCandidate struct {
	Code         string
	VarsTemplate string
}

// FirmwarePathCandidates lists where UEFI firmware lives on supported hosts,
// keyed by Architecture string ("x86_64" | "arm64"). First match wins so
// distro-native packages take precedence over edk2 fallbacks.
//
// Exported so tests across packages can swap entries via t.Cleanup. The RHEL
// host plan extends this map by appending candidates.
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

// FirmwarePaths returns the first (code, varsTemplate, varsSize) tuple from
// FirmwarePathCandidates[arch] where both files exist on disk. varsSize is
// the on-disk size of the VARS template — QEMU pflash requires the per-VM
// VARS volume to match the template byte-for-byte, so the caller sizes the
// viperblock volume from this return.
//
// Returns an error mentioning the arch on miss; the launch path treats this
// as a hard failure rather than falling back to SeaBIOS (a UEFI guest booted
// under SeaBIOS panics on missing ESP).
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
