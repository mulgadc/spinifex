package daemon

import (
	"context"
	"log/slog"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

func (d *Daemon) handleEC2PutSpotInstanceRequests(msg *nats.Msg) {
	handleNATSRequest(msg, d.spotInstanceService.PutSpotInstanceRequests)
}

func (d *Daemon) handleEC2DescribeSpotInstanceRequests(msg *nats.Msg) {
	handleNATSRequest(msg, d.spotInstanceService.DescribeSpotInstanceRequests)
}

func (d *Daemon) handleEC2CancelSpotInstanceRequests(msg *nats.Msg) {
	handleNATSRequest(msg, d.spotInstanceService.CancelSpotInstanceRequests)
}

// handleSetSpotLineage stamps the spot lineage (InstanceLifecycle=spot +
// SpotInstanceRequestId) onto a launched VM. Dispatched from handleEC2Events,
// which already verified ownership; the SIR id is the only datum on the wire as
// the lifecycle is always spot for this command.
func (d *Daemon) handleSetSpotLineage(ctx context.Context, msg *nats.Msg, command types.EC2InstanceCommand) {
	if command.SpotLineageData == nil {
		respondWithError(msg, awserrors.ErrorMissingParameter)
		return
	}

	found, err := d.vmMgr.UpdateAndPersist(command.ID, func(v *vm.VM) bool {
		v.InstanceLifecycle = ec2.InstanceLifecycleTypeSpot
		v.SpotInstanceRequestId = command.SpotLineageData.SpotInstanceRequestId
		return true
	})
	if err != nil {
		respondWithError(msg, awserrors.ValidErrorCode(err.Error()))
		return
	}
	if !found {
		respondWithError(msg, awserrors.ErrorInvalidInstanceIDNotFound)
		return
	}

	if err := msg.Respond([]byte(`{}`)); err != nil {
		slog.ErrorContext(ctx, "Failed to respond to NATS request", "err", err)
	}
}
