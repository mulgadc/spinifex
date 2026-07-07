package gateway_ec2_instance

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// runIntoReservation routes a targeted launch straight to the owning daemon's
// per-reservation subject. Only the owner subscribes ec2.RunInstances.cr.<crID>,
// so NATS addresses it without any cr→node lookup. ErrNoResponders means the
// reservation is gone (cancelled or lost to a restart) → NotFound; any semantic
// error (type mismatch, full) rides back as the daemon's awserror code.
func runIntoReservation(ctx context.Context, input *ec2.RunInstancesInput, natsConn *nats.Conn, accountID, crID string) (*ec2.Reservation, error) {
	subject := "ec2.RunInstances.cr." + crID
	reservation, err := utils.NATSRequestCtx[ec2.Reservation](ctx, natsConn, subject, input, 5*time.Minute, accountID)
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) {
			return nil, errors.New(awserrors.ErrorInvalidCapacityReservationIdNotFound)
		}
		return nil, err
	}
	return reservation, nil
}
