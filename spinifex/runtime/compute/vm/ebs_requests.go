package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// EBSRequests is the VM-owned attachment collection. Its mutex protects the
// persisted request list while EC2 lifecycle work, runtime volume operations
// and recovery inspect it concurrently.
type EBSRequests struct {
	Requests []EBSRequest `json:"Requests" mapstructure:"ebs_requests"`
	Mu       sync.Mutex   `json:"-"`
}

// EBSRequest is one volume in the VM's persisted attachment list. Instance
// records and local state store it as JSON, so its field names and tags are
// persisted state.
type EBSRequest struct {
	Name                string `json:"Name"`
	VolType             string `json:"VolType"`
	Boot                bool   `json:"Boot"`
	EFI                 bool   `json:"EFI"`
	DeleteOnTermination bool   `json:"DeleteOnTermination"`
	NBDURI              string `json:"NBDURI"`
	DeviceName          string `json:"DeviceName"`
	HotplugPort         int    `json:"HotplugPort,omitempty"`
	// BlockDevice and BlockSizeBytes describe a host block device export, the
	// alternative to NBDURI; the guest sees BlockSizeBytes of it. Exactly one
	// of NBDURI and BlockDevice may be set.
	BlockDevice    string `json:"BlockDevice,omitempty"`
	BlockSizeBytes uint64 `json:"BlockSizeBytes,omitempty"`
}

// volumeExportKind is how a volume's backing store reaches QEMU.
type volumeExportKind int

const (
	volumeExportInvalid volumeExportKind = iota
	volumeExportNBD
	volumeExportBlock
)

// classifyVolumeExport reports which export a request carries. A block device
// must be absolute, comma-free (QEMU key=value options would split it) and
// sized; both exports set, neither, or a stray size is invalid.
func classifyVolumeExport(req EBSRequest) (volumeExportKind, error) {
	switch {
	case req.NBDURI != "" && req.BlockDevice != "":
		return volumeExportInvalid, fmt.Errorf("volume %s has both NBDURI and BlockDevice set", req.Name)
	case req.BlockSizeBytes != 0 && req.BlockDevice == "":
		return volumeExportInvalid, fmt.Errorf("volume %s has BlockSizeBytes set without BlockDevice", req.Name)
	case req.NBDURI != "":
		return volumeExportNBD, nil
	case req.BlockDevice != "":
		if !filepath.IsAbs(req.BlockDevice) {
			return volumeExportInvalid, fmt.Errorf("volume %s BlockDevice %q is not an absolute path", req.Name, req.BlockDevice)
		}
		if strings.Contains(req.BlockDevice, ",") {
			return volumeExportInvalid, fmt.Errorf("volume %s BlockDevice %q contains a comma", req.Name, req.BlockDevice)
		}
		if req.BlockSizeBytes == 0 {
			return volumeExportInvalid, fmt.Errorf("volume %s BlockDevice %q has zero BlockSizeBytes", req.Name, req.BlockDevice)
		}
		return volumeExportBlock, nil
	default:
		return volumeExportInvalid, fmt.Errorf("NBDURI not set for volume %s - was volume mounted?", req.Name)
	}
}

// checkBlockDevice reports whether a block export's device node is present.
func checkBlockDevice(path string) error {
	_, err := os.Stat(path)
	return err
}
