package daemon

import (
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_image "github.com/mulgadc/spinifex/spinifex/handlers/ec2/image"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

func (d *Daemon) handleEC2DescribeImages(msg *nats.Msg) {
	handleNATSRequest(msg, d.imageService.DescribeImages)
}

func (d *Daemon) handleEC2DeregisterImage(msg *nats.Msg) {
	handleNATSRequest(msg, d.imageService.DeregisterImage)
}

func (d *Daemon) handleEC2RegisterImage(msg *nats.Msg) {
	handleNATSRequest(msg, d.imageService.RegisterImage)
}

func (d *Daemon) handleEC2CopyImage(msg *nats.Msg) {
	handleNATSRequest(msg, d.imageService.CopyImage)
}

func (d *Daemon) handleEC2DescribeImageAttribute(msg *nats.Msg) {
	handleNATSRequest(msg, d.imageService.DescribeImageAttribute)
}

func (d *Daemon) handleEC2ModifyImageAttribute(msg *nats.Msg) {
	handleNATSRequest(msg, d.imageService.ModifyImageAttribute)
}

func (d *Daemon) handleEC2ResetImageAttribute(msg *nats.Msg) {
	handleNATSRequest(msg, d.imageService.ResetImageAttribute)
}

func (d *Daemon) handleSpinifexPromoteImage(msg *nats.Msg) {
	promoteImage := func(input *admin.PromoteImageOpts, _ string) (*admin.PromoteImageResult, error) {
		store := objectstore.NewS3ObjectStoreFromConfig(
			admin.DialTarget(d.config.Predastore.Host),
			d.config.Predastore.Region,
			d.config.Predastore.AccessKey,
			d.config.Predastore.SecretKey,
		)
		return admin.PromoteSystemImage(store, d.config.Predastore.Bucket, *input)
	}
	handleNATSRequest(msg, promoteImage)
}

// handleEC2CreateImage is a stateful handler that extracts instance context
// (root volume ID, source AMI, running state) before delegating to the image service.
func (d *Daemon) handleEC2CreateImage(msg *nats.Msg) {
	slog.Debug("Received message", "subject", msg.Subject)

	input := &ec2.CreateImageInput{}
	if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
		return
	}

	accountID := utils.AccountIDFromMsg(msg)

	if input.InstanceId == nil || *input.InstanceId == "" {
		respondWithError(msg, awserrors.ErrorMissingParameter)
		return
	}

	instanceID := *input.InstanceId

	// Extract all instance context in a single critical section
	var (
		instance      *vm.VM
		status        vm.InstanceState
		rootVolumeID  string
		sourceImageID string
	)
	ok := d.vmMgr.UpdateState(instanceID, func(v *vm.VM) {
		instance = v
		status = v.Status
		if v.Instance != nil {
			for _, bdm := range v.Instance.BlockDeviceMappings {
				if bdm.Ebs != nil && bdm.Ebs.VolumeId != nil {
					rootVolumeID = *bdm.Ebs.VolumeId
					break
				}
			}
			if v.Instance.ImageId != nil {
				sourceImageID = *v.Instance.ImageId
			}
		}
	})

	if !ok {
		// Stopped instances are migrated out of the local map into the
		// cluster-shared KV bucket when they stop — check there too.
		var stopped *vm.VM
		if d.stateStore != nil {
			var err error
			stopped, err = d.stateStore.LoadStoppedInstance(instanceID)
			if err != nil {
				slog.Warn("CreateImage: error loading stopped instance", "instanceId", instanceID, "err", err)
			}
		}
		if stopped == nil {
			slog.Warn("CreateImage: instance not found", "instanceId", instanceID)
			respondWithError(msg, awserrors.ErrorInvalidInstanceIDNotFound)
			return
		}
		instance = stopped
		status = stopped.Status
		if stopped.Instance != nil {
			for _, bdm := range stopped.Instance.BlockDeviceMappings {
				if bdm.Ebs != nil && bdm.Ebs.VolumeId != nil {
					rootVolumeID = *bdm.Ebs.VolumeId
					break
				}
			}
			if stopped.Instance.ImageId != nil {
				sourceImageID = *stopped.Instance.ImageId
			}
		}
	}

	// Verify the caller owns this instance
	if !checkInstanceOwnership(msg, instanceID, instance.AccountID) {
		return
	}

	if status != vm.StateRunning && status != vm.StateStopped {
		slog.Warn("CreateImage: instance not in valid state", "instanceId", instanceID, "status", status)
		respondWithError(msg, awserrors.ErrorIncorrectInstanceState)
		return
	}

	if rootVolumeID == "" {
		slog.Error("CreateImage: no root volume found", "instanceId", instanceID)
		respondWithError(msg, awserrors.ErrorServerInternal)
		return
	}

	params := handlers_ec2_image.CreateImageParams{
		Input:         input,
		RootVolumeID:  rootVolumeID,
		SourceImageID: sourceImageID,
		IsRunning:     status == vm.StateRunning,
	}

	output, err := d.imageService.CreateImageFromInstance(params, accountID)
	if err != nil {
		slog.Error("CreateImage: service failed", "instanceId", instanceID, "err", err)
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}

	respondWithJSON(msg, output)
	slog.Info("CreateImage completed", "instanceId", instanceID, "imageId", *output.ImageId)
}
