package instance

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// reconcileStartVolumes drops the data volumes a claimed instance can no longer
// have, and fails the start when its boot volume is gone.
func (s *InstanceServiceImpl) reconcileStartVolumes(ctx context.Context, instance *vm.VM) error {
	// A direct-boot system guest boots from a kernel, not a root volume, and its
	// one data volume, if any, is its own store rather than a customer's.
	if s.metadata == nil || instance.DirectBoot {
		return nil
	}

	instance.EBSRequests.Mu.Lock()
	requests := slices.Clone(instance.EBSRequests.Requests)
	instance.EBSRequests.Mu.Unlock()

	if !slices.ContainsFunc(requests, func(r vm.EBSRequest) bool { return r.Boot }) {
		return noRootVolumeError(instance)
	}

	var drop []string
	for _, req := range requests {
		meta, err := s.metadata.GetVolume(ctx, instance.AccountID, req.Name)
		missing := err != nil && objectstore.IsNoSuchKeyError(err)
		if err != nil && !missing {
			return fmt.Errorf("read volume %s: %w", req.Name, err)
		}
		if req.Boot {
			if missing {
				slog.ErrorContext(ctx, "start: root volume no longer exists", "instanceId", instance.ID, "volumeId", req.Name)
				return noRootVolumeError(instance)
			}
			continue
		}
		if req.EFI {
			continue
		}
		if missing {
			slog.WarnContext(ctx, "start: dropping deleted data volume", "instanceId", instance.ID, "volumeId", req.Name)
			drop = append(drop, req.Name)
			continue
		}
		if meta.AttachedInstance != "" && meta.AttachedInstance != instance.ID {
			slog.WarnContext(ctx, "start: dropping data volume attached to another instance",
				"instanceId", instance.ID, "volumeId", req.Name, "attachedTo", meta.AttachedInstance)
			drop = append(drop, req.Name)
		}
	}
	if len(drop) == 0 {
		return nil
	}

	instance.EBSRequests.Mu.Lock()
	instance.EBSRequests.Requests = slices.DeleteFunc(instance.EBSRequests.Requests, func(r vm.EBSRequest) bool {
		return slices.Contains(drop, r.Name)
	})
	instance.EBSRequests.Mu.Unlock()
	if instance.Instance != nil {
		instance.Instance.BlockDeviceMappings = slices.DeleteFunc(instance.Instance.BlockDeviceMappings, func(m *ec2.InstanceBlockDeviceMapping) bool {
			return m != nil && m.Ebs != nil && slices.Contains(drop, aws.StringValue(m.Ebs.VolumeId))
		})
	}
	return nil
}

// noRootVolumeError is what AWS answers when an instance is started with
// nothing attached at its root device name.
func noRootVolumeError(instance *vm.VM) error {
	device := ebsmetadata.RootDeviceName
	if instance.Instance != nil && aws.StringValue(instance.Instance.RootDeviceName) != "" {
		device = aws.StringValue(instance.Instance.RootDeviceName)
	}
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "Instance does not have a volume attached at root (%s)", device)
}
