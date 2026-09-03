package daemon

import (
	"context"
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

func (d *Daemon) handleSpinifexPromoteImage(msg *nats.Msg) string {
	promoteImage := func(_ context.Context, input *admin.PromoteImageOpts, _ string) (*admin.PromoteImageResult, error) {
		store := objectstore.NewS3ObjectStoreFromConfig(
			admin.DialTarget(d.config.Predastore.Host),
			d.config.Predastore.Region,
			d.config.Predastore.AccessKey,
			d.config.Predastore.SecretKey,
		)
		return admin.PromoteSystemImage(store, d.config.Predastore.Bucket, *input)
	}
	return handleNATSRequest(d.node, promoteImage)(msg)
}

// stoppedInstanceOwnerElector is implemented by state stores that can
// atomically mutate a stopped instance record. Checked via assertion so
// vm.StateStore itself need not widen for this one call site.
type stoppedInstanceOwnerElector interface {
	UpdateStoppedInstance(id string, mutate func(*vm.VM)) (*vm.VM, error)
}

// handleEC2CreateImage is a stateful handler that extracts instance context
// (root volume ID, source AMI, running state) before delegating to the image service.
func (d *Daemon) handleEC2CreateImage(msg *nats.Msg) string {
	slog.Debug("Received message", "subject", msg.Subject)

	input := &ec2.CreateImageInput{}
	if errResp := utils.UnmarshalJsonPayload(input, msg.Data); errResp != nil {
		if err := msg.Respond(errResp); err != nil {
			slog.Error("Failed to respond to NATS request", "err", err)
		}
		return outcomeError
	}

	accountID := utils.AccountIDFromMsg(msg)

	if input.InstanceId == nil || *input.InstanceId == "" {
		respondWithError(d.node, msg, awserrors.ErrorMissingParameter)
		return outcomeError
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
			respondWithError(d.node, msg, awserrors.ErrorInvalidInstanceIDNotFound)
			return outcomeError
		}

		// The KV is cluster-shared, so every node's lookup above succeeds;
		// only LastNode picks a single owner. Non-owners decline like the
		// miss above — Gather only short-circuits on success, so this can't
		// preempt the owner's reply, it just stops a duplicate AMI/snapshot.
		if stopped.LastNode == "" {
			// Pre-LastNode record from an older build. Elect an owner via
			// CAS so exactly one node wins, instead of every node
			// proceeding (the leak) or every node declining forever.
			elector, canElect := d.stateStore.(stoppedInstanceOwnerElector)
			if !canElect {
				slog.Error("CreateImage: state store cannot elect owner for legacy stopped instance", "instanceId", instanceID)
				respondWithError(d.node, msg, awserrors.ErrorInvalidInstanceIDNotFound)
				return outcomeError
			}
			elected, uerr := elector.UpdateStoppedInstance(instanceID, func(v *vm.VM) {
				if v.LastNode == "" {
					v.LastNode = d.node
				}
			})
			if uerr != nil || elected == nil || elected.LastNode == "" {
				slog.Warn("CreateImage: failed to elect owner for legacy stopped instance", "instanceId", instanceID, "err", uerr)
				respondWithError(d.node, msg, awserrors.ErrorInvalidInstanceIDNotFound)
				return outcomeError
			}
			stopped.LastNode = elected.LastNode
		}
		if stopped.LastNode != d.node {
			slog.Info("CreateImage: declining, instance owned by another node",
				"instanceId", instanceID, "lastNode", stopped.LastNode)
			respondWithError(d.node, msg, awserrors.ErrorInvalidInstanceIDNotFound)
			return outcomeError
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
	if !checkInstanceOwnership(d.node, msg, instanceID, instance.AccountID) {
		return outcomeError
	}

	if status != vm.StateRunning && status != vm.StateStopped {
		slog.Warn("CreateImage: instance not in valid state", "instanceId", instanceID, "status", status)
		respondWithError(d.node, msg, awserrors.ErrorIncorrectInstanceState)
		return outcomeError
	}

	if rootVolumeID == "" {
		slog.Error("CreateImage: no root volume found", "instanceId", instanceID)
		respondWithError(d.node, msg, awserrors.ErrorServerInternal)
		return outcomeError
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
		respondWithServiceError(d.node, msg, err)
		return outcomeError
	}

	respondWithJSON(d.node, msg, output)
	slog.Info("CreateImage completed", "instanceId", instanceID, "imageId", *output.ImageId)
	return outcomeSuccess
}
