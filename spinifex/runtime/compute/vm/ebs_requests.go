package vm

import (
	"sync"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
)

// EBSRequests is the VM-owned attachment collection. Its mutex protects the
// persisted request list while EC2 lifecycle work, runtime volume operations
// and recovery inspect it concurrently. The individual entries retain the
// legacy Viperblockd wire shape because the daemon adapter sends them on the
// deployed mount and unmount routes.
type EBSRequests struct {
	Requests []EBSRequest `json:"Requests" mapstructure:"ebs_requests"`
	Mu       sync.Mutex   `json:"-"`
}

// EBSRequest is the legacy Viperblockd mount/unmount payload retained in the
// VM's persisted attachment list. The alias keeps the VM state tied to the
// deployed wire shape without making the old catch-all types package an owner.
type EBSRequest = viperblocklegacyv1.EBSRequest
