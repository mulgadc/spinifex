package instance

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// reconcileStartVolumes drops the data volumes a claimed instance can no longer
// have, and fails the start when its boot volume is gone.
func (s *InstanceServiceImpl) reconcileStartVolumes(ctx context.Context, instance *vm.VM) error {
	if s.metadata == nil {
		return nil
	}

	instance.EBSRequests.Mu.Lock()
	requests := slices.Clone(instance.EBSRequests.Requests)
	instance.EBSRequests.Mu.Unlock()

	var drop []string
	for _, req := range requests {
		meta, err := s.metadata.GetVolume(ctx, instance.AccountID, req.Name)
		missing := err != nil && objectstore.IsNoSuchKeyError(err)
		if err != nil && !missing {
			return fmt.Errorf("read volume %s: %w", req.Name, err)
		}
		if req.Boot {
			if missing {
				return fmt.Errorf("boot volume %s of %s no longer exists", req.Name, instance.ID)
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
	instance.EBSRequests.Requests = slices.DeleteFunc(instance.EBSRequests.Requests, func(r viperblocklegacyv1.EBSRequest) bool {
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

