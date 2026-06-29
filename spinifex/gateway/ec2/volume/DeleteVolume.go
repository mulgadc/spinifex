package gateway_ec2_volume

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	handlers_ec2_volume "github.com/mulgadc/spinifex/spinifex/handlers/ec2/volume"
	"github.com/nats-io/nats.go"
)

func ValidateDeleteVolumeInput(input *ec2.DeleteVolumeInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.VolumeId == nil || len(*input.VolumeId) <= len("vol-") || !strings.HasPrefix(*input.VolumeId, "vol-") {
		return errors.New(awserrors.ErrorInvalidVolumeIDMalformed)
	}

	return nil
}

// volumeHeldByInstance returns the id of a non-terminated instance whose
// BlockDeviceMappings reference volumeID, or "" if none does. A terminated
// instance's mappings are stale (its volumes are released), so they do not
// block a delete. This is the authoritative in-use signal: persisted volume
// State/AttachedInstance can drift out of sync with the real attachment.
func volumeHeldByInstance(reservations []*ec2.Reservation, volumeID string) string {
	for _, res := range reservations {
		if res == nil {
			continue
		}
		for _, inst := range res.Instances {
			if inst == nil {
				continue
			}
			if inst.State != nil && aws.StringValue(inst.State.Name) == ec2.InstanceStateNameTerminated {
				continue
			}
			for _, bdm := range inst.BlockDeviceMappings {
				if bdm != nil && bdm.Ebs != nil && aws.StringValue(bdm.Ebs.VolumeId) == volumeID {
					return aws.StringValue(inst.InstanceId)
				}
			}
		}
	}
	return ""
}

// DeleteVolume handles the DeleteVolume API call.
//
// Before destroying the backing it cross-checks live cluster-wide instance
// BlockDeviceMappings: the daemon-side gate trusts persisted volume metadata,
// which the attach/detach lifecycle can leave desynced (a running root with
// State:"" / AttachedInstance:""), so a drifted-but-live volume could be
// deleted under a running QEMU. The check fails closed on an incomplete view.
func DeleteVolume(input *ec2.DeleteVolumeInput, natsConn *nats.Conn, expectedNodes int, accountID string) (ec2.DeleteVolumeOutput, error) {
	var output ec2.DeleteVolumeOutput

	err := ValidateDeleteVolumeInput(input)
	if err != nil {
		return output, err
	}

	volumeID := aws.StringValue(input.VolumeId)

	// Strict fan-out: complete is true only when every active node answered and
	// both instance buckets queried, so a node that owns the attachment cannot be
	// silently missing from the survey. Refuse rather than delete against a
	// partial view of the cluster.
	reservations, complete, err := gateway_ec2_instance.DescribeInstancesForReconcile(
		&ec2.DescribeInstancesInput{}, natsConn, expectedNodes, accountID)
	if err != nil {
		slog.Error("DeleteVolume: in-use precheck fan-out failed", "volumeId", volumeID, "err", err)
		return output, errors.New(awserrors.ErrorServerInternal)
	}
	if !complete {
		slog.Error("DeleteVolume: refusing delete on incomplete instance view", "volumeId", volumeID)
		return output, errors.New(awserrors.ErrorServerInternal)
	}
	if holder := volumeHeldByInstance(reservations, volumeID); holder != "" {
		slog.Error("DeleteVolume: volume still attached to a live instance",
			"volumeId", volumeID, "instanceId", holder)
		return output, errors.New(awserrors.ErrorVolumeInUse)
	}

	volumeService := handlers_ec2_volume.NewNATSVolumeService(natsConn)
	result, err := volumeService.DeleteVolume(input, accountID)

	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
