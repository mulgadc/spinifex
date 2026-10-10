package vm

import (
	"context"
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
	// Unmount sends ebs.unmount for each attached volume, which drives the
	// synchronous block-map seal. Every volume is attempted; the seal
	// failures are aggregated and returned, and mean the data is not durable.
	Unmount(ctx context.Context, v *VM) error

	// Abandon gives up this node's export of every attached volume without
	// sealing, for an instance another node owns now.
	//
	// It is not an unmount and must never become one. A seal here publishes this
	// node's stale block map over the winner's. It is also not optional: the
	// export holds the volume lease, and a healthy node renews that lease
	// forever, so a guest stopped here without this is a guest that cannot start
	// anywhere.
	Abandon(ctx context.Context, v *VM, reason string) error

	// MountOne sends ebs.mount for a single request and writes the resolved
	// NBDURI back into req.NBDURI on success. Used by hot-attach. accountID
	// names the owner, which a lone request carries no instance to supply.
	MountOne(ctx context.Context, accountID string, req *EBSRequest) error
	// UnmountOne sends ebs.unmount for a single request and returns any error.
	// ebs.unmount drives the synchronous block-map seal to predastore, so hot
	// detach gates the volume's available transition on the returned error.
	UnmountOne(ctx context.Context, accountID string, req EBSRequest) error
}
