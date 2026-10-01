package viperblocklegacyv1

import "fmt"

const (
	// DeleteSubject is the legacy queue-group route that deletes a volume's
	// Viperblockd-local state and any active export.
	DeleteSubject = "ebs.delete"

	// MountResponseSubject is the legacy broadcast emitted after an ebs.mount
	// request is handled. Request/reply callers receive the same payload.
	MountResponseSubject = "ebs.mount.response"

	// UnmountResponseSubject is the legacy broadcast emitted after an
	// ebs.unmount request is handled. Request/reply callers receive the same
	// payload.
	UnmountResponseSubject = "ebs.unmount.response"
)

// MountSubject returns the deployed mount route. An empty node retains the
// single-node queue-group route; a named node selects the Viperblockd worker
// colocated with the instance runtime.
func MountSubject(node string) string {
	if node == "" {
		return "ebs.mount"
	}
	return fmt.Sprintf("ebs.%s.mount", node)
}

// UnmountSubject returns the deployed unmount route. Its empty-node behaviour
// matches MountSubject for single-node compatibility.
func UnmountSubject(node string) string {
	if node == "" {
		return "ebs.unmount"
	}
	return fmt.Sprintf("ebs.%s.unmount", node)
}

// EBSRequest is the deployed request payload shared by the legacy mount and
// unmount routes. It is intentionally separate from the generic EBS provider
// contract: this is Viperblockd's transitional NATS protocol.
type EBSRequest struct {
	Name                string `json:"Name"`
	VolType             string `json:"VolType"`
	Boot                bool   `json:"Boot"`
	EFI                 bool   `json:"EFI"`
	DeleteOnTermination bool   `json:"DeleteOnTermination"`
	NBDURI              string `json:"NBDURI"`
	DeviceName          string `json:"DeviceName"`
	HotplugPort         int    `json:"HotplugPort,omitempty"`
}

// EBSMountResponse is the response to an EBSRequest on MountSubject.
type EBSMountResponse struct {
	URI       string `json:"URI"`
	Mounted   bool   `json:"Mounted"`
	Error     string `json:"Error"`
	Retryable bool   `json:"Retryable"`
}

// EBSUnMountResponse is the response to an EBSRequest on UnmountSubject.
// NotFound reports an idempotent retry after an already-complete seal; Reaped
// reports a leaked export that was cleaned up without a registry entry.
type EBSUnMountResponse struct {
	Volume   string `json:"Volume"`
	Mounted  bool   `json:"Mounted"`
	Error    string `json:"Error"`
	NotFound bool   `json:"NotFound"`
	Reaped   bool   `json:"Reaped,omitempty"`
}

// EBSDeleteRequest identifies the volume to delete on DeleteSubject.
type EBSDeleteRequest struct {
	Volume string `json:"Volume"`
}

// EBSDeleteResponse is the response to EBSDeleteRequest on DeleteSubject.
type EBSDeleteResponse struct {
	Volume  string `json:"Volume"`
	Success bool   `json:"Success"`
	Error   string `json:"Error"`
}
