package vm

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/types"
)

// VolumeMounter mounts and unmounts the EBS volumes attached to a VM. The
// real implementation routes ebs.mount / ebs.unmount NATS requests; the
// abstraction keeps NATS out of the manager.
//
// Every method takes the caller's context so the ebs.* span joins the trace
// that asked for the work rather than rooting one of its own. Shutdown and
// crash recovery have no caller and pass a background context, which is an
// honest answer rather than a missing one.
type VolumeMounter interface {
	// Mount mounts every attached volume in v.EBSRequests.Requests, recording
	// the resolved NBDURI back onto each request entry.
	Mount(ctx context.Context, v *VM) error
	// Unmount sends ebs.unmount for each attached volume. Errors are logged
	// per volume and aggregated; partial failure is tolerated.
	Unmount(ctx context.Context, v *VM) error

	// MountOne sends ebs.mount for a single request and writes the resolved
	// NBDURI back into req.NBDURI on success. Used by hot-attach. accountID
	// names the owner, which a lone request carries no instance to supply.
	MountOne(ctx context.Context, accountID string, req *types.EBSRequest) error
	// UnmountOne sends ebs.unmount for a single request and returns any error.
	// ebs.unmount drives the synchronous block-map seal to predastore, so hot
	// detach gates the volume's available transition on the returned error.
	UnmountOne(ctx context.Context, accountID string, req types.EBSRequest) error
}
