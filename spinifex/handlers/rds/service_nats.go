package handlers_rds

import (
	"context"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const defaultTimeout = 30 * time.Second

// The gateway-side adapter that forwards each agent action as a NATS request to
// the daemon's matching subscriber.
type NATSService struct {
	nc *nats.Conn
}

func NewNATSService(nc *nats.Conn) *NATSService {
	return &NATSService{nc: nc}
}

func (s *NATSService) RegisterDBInstance(ctx context.Context, input *RegisterDBInstanceInput, accountID string) (*RegisterDBInstanceOutput, error) {
	return utils.NATSRequest[RegisterDBInstanceOutput](ctx, s.nc,
		BusRegisterSubject(accountID, input.DBInstanceIdentifier), input, defaultTimeout, accountID)
}

func (s *NATSService) SubmitDBStateChange(ctx context.Context, input *SubmitDBStateChangeInput, accountID string) (*SubmitDBStateChangeOutput, error) {
	return utils.NATSRequest[SubmitDBStateChangeOutput](ctx, s.nc,
		BusHealthSubject(accountID, input.DBInstanceIdentifier), input, defaultTimeout, accountID)
}

// Requested on the Layer-1 subject, not the bus.
func (s *NATSService) GetDBBootstrapConfig(ctx context.Context, input *GetDBBootstrapConfigInput, accountID string) (*GetDBBootstrapConfigOutput, error) {
	return utils.NATSRequest[GetDBBootstrapConfigOutput](ctx, s.nc,
		SubjectGetDBBootstrapConfig, input, defaultTimeout, accountID)
}
