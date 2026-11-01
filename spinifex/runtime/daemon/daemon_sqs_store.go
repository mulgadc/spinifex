package daemon

import (
	"errors"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// sqsDataDevice is the device name an SQS guest's data volume is recorded
// under; the guest finds the disk by serial, not by this name.
const sqsDataDevice = "/dev/sdf"

// sqsGuest is an SQS direct-boot guest launch that has passed every check
// that can run before anything is allocated.
type sqsGuest interface {
	imageDir() string
	dataVolume() string
	needsMgmtNIC() bool
	label() string
	writeFwCfg(instanceID string) ([]vm.FwCfgEntry, error)
	// record stores what a same-node relaunch rebuilds the fw_cfg files from.
	record(inst *vm.VM, nodeID, netcfg string)
}

// sqsStoreMinMemoryMiB is the heap limit the guest's SQS NATS server runs
// under; a guest no larger than it cannot also hold the worker, its kernel
// and page cache. This is a floor, not a validated size.
const sqsStoreMinMemoryMiB = 512

// ErrSQSUndersized refuses an instance type too small for the SQS guest.
var ErrSQSUndersized = errors.New("sqs: instance type memory does not exceed the SQS guest store's 512 MiB heap limit")

func sqsMemoryFits(name string, it *ec2.InstanceTypeInfo) error {
	if it == nil || instanceTypeMemoryMiB(it) <= sqsStoreMinMemoryMiB {
		return fmt.Errorf("%w: %s", ErrSQSUndersized, name)
	}
	return nil
}

// sqsNetcfgOnly keeps the network config and drops the load balancer's agent
// environment and CA bundle, which an SQS guest must not receive.
func sqsNetcfgOnly(entries []vm.FwCfgEntry) []vm.FwCfgEntry {
	kept := make([]vm.FwCfgEntry, 0, 1)
	for _, e := range entries {
		if e.Name == "opt/spinifex/netcfg" {
			kept = append(kept, e)
			continue
		}
		_ = os.Remove(e.File)
	}
	return kept
}

// refuseSQSStoreRelaunch refuses an SQS store VM recorded by an earlier
// release: this release runs no separate store. Recovery marks it failed and
// keeps its data volume; nothing is deleted.
func refuseSQSStoreRelaunch(inst *vm.VM) error {
	return fmt.Errorf("sqs store instance %s is not supported by this release and is not relaunched; its data volume is kept", inst.ID)
}

// sqsKeepsDataVolume reports whether the record mounts the data volume
// through the normal EBS path and keeps it on terminate.
func sqsKeepsDataVolume(inst *vm.VM, volumeID string) bool {
	inst.EBSRequests.Mu.Lock()
	defer inst.EBSRequests.Mu.Unlock()
	for _, r := range inst.EBSRequests.Requests {
		if r.Name == volumeID {
			return !r.DeleteOnTermination
		}
	}
	return false
}
