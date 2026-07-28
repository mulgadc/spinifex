package handlers_rds

import (
	"context"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const defaultTimeout = 30 * time.Second

// NATSService is the gateway-side adapter that forwards each agent action as a
// NATS request to the daemon's matching subscriber.
type NATSService struct {
	nc *nats.Conn
}

// NewNATSService returns the gateway-side RDS client.
func NewNATSService(nc *nats.Conn) *NATSService {
	return &NATSService{nc: nc}
}

// RegisterDBInstance relays onto the instance's bus register subject.
func (s *NATSService) RegisterDBInstance(ctx context.Context, input *RegisterDBInstanceInput, accountID string) (*RegisterDBInstanceOutput, error) {
	return utils.NATSRequest[RegisterDBInstanceOutput](ctx, s.nc,
		BusRegisterSubject(accountID, input.DBInstanceIdentifier), input, defaultTimeout, accountID)
}

// SubmitDBStateChange relays the beat onto the instance's bus health subject.
func (s *NATSService) SubmitDBStateChange(ctx context.Context, input *SubmitDBStateChangeInput, accountID string) (*SubmitDBStateChangeOutput, error) {
	return utils.NATSRequest[SubmitDBStateChangeOutput](ctx, s.nc,
		BusHealthSubject(accountID, input.DBInstanceIdentifier), input, defaultTimeout, accountID)
}

// GetDBBootstrapConfig requests bootstrap material on the Layer-1 subject.
func (s *NATSService) GetDBBootstrapConfig(ctx context.Context, input *GetDBBootstrapConfigInput, accountID string) (*GetDBBootstrapConfigOutput, error) {
	return utils.NATSRequest[GetDBBootstrapConfigOutput](ctx, s.nc,
		SubjectGetDBBootstrapConfig, input, defaultTimeout, accountID)
}
