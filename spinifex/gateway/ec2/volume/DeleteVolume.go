package gateway_ec2_volume

import (
	"context"
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

// ValidateDeleteVolumeInput rejects a nil input with InvalidParameterValue and a VolumeId that is
// missing or not a non-empty vol- ID with InvalidVolumeID.Malformed.
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
// deleted under a running QEMU. The check fails closed unless every node in
// nodeIDs answered by identity, so a duplicate reply cannot stand in for a
// silent node.
func DeleteVolume(ctx context.Context, input *ec2.DeleteVolumeInput, natsConn *nats.Conn, nodeIDs []string, accountID string) (ec2.DeleteVolumeOutput, error) {
	var output ec2.DeleteVolumeOutput

	err := ValidateDeleteVolumeInput(input)
	if err != nil {
		return output, err
	}

	volumeID := aws.StringValue(input.VolumeId)

	// Every configured node and both instance buckets must answer, judged by
	// responder identity, or the node owning the attachment could be the one
	// missing. Refuse rather than delete against a partial view of the cluster.
	listing, err := gateway_ec2_instance.DescribeAccountInstancesComplete(ctx, natsConn, nodeIDs, accountID)
	if err != nil {
		slog.ErrorContext(ctx, "DeleteVolume: refusing delete without a complete instance view", "volumeId", volumeID, "err", err)
		// An unanswered node is retryable once the cluster is whole again, so
		// keep the ServiceUnavailable and its Retry-After; anything else is ours.
		if code, ok := awserrors.ResolveErrorCode(err); ok && code == awserrors.ErrorServiceUnavailable {
			return output, err
		}
		return output, errors.New(awserrors.ErrorServerInternal)
	}
	if holder := volumeHeldByInstance(listing.Reservations, volumeID); holder != "" {
		slog.ErrorContext(ctx, "DeleteVolume: volume still attached to a live instance",
			"volumeId", volumeID, "instanceId", holder)
		return output, errors.New(awserrors.ErrorVolumeInUse)
	}

	volumeService := handlers_ec2_volume.NewNATSVolumeService(natsConn)
	result, err := volumeService.DeleteVolume(ctx, input, accountID)

	if err != nil {
		return output, err
	}

	output = *result
	return output, nil
}
