package handlers_rds

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const defaultTimeout = 30 * time.Second

// A create provisions an ENI, a VM and a volume inline, so it needs far more
// than the agent protocol's round-trip budget.
const createTimeout = 5 * time.Minute

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

func (s *NATSService) CreateDBInstance(ctx context.Context, input *rds.CreateDBInstanceInput, accountID string) (*rds.CreateDBInstanceOutput, error) {
	return utils.NATSRequest[rds.CreateDBInstanceOutput](ctx, s.nc,
		SubjectCreateDBInstance, input, createTimeout, accountID)
}

func (s *NATSService) DescribeDBInstances(ctx context.Context, input *rds.DescribeDBInstancesInput, accountID string) (*rds.DescribeDBInstancesOutput, error) {
	return utils.NATSRequest[rds.DescribeDBInstancesOutput](ctx, s.nc,
		SubjectDescribeDBInstances, input, defaultTimeout, accountID)
}

func (s *NATSService) AddTagsToResource(ctx context.Context, input *rds.AddTagsToResourceInput, accountID string) (*rds.AddTagsToResourceOutput, error) {
	return utils.NATSRequest[rds.AddTagsToResourceOutput](ctx, s.nc,
		SubjectAddTagsToResource, input, defaultTimeout, accountID)
}

func (s *NATSService) RemoveTagsFromResource(ctx context.Context, input *rds.RemoveTagsFromResourceInput, accountID string) (*rds.RemoveTagsFromResourceOutput, error) {
	return utils.NATSRequest[rds.RemoveTagsFromResourceOutput](ctx, s.nc,
		SubjectRemoveTagsFromResource, input, defaultTimeout, accountID)
}

func (s *NATSService) ListTagsForResource(ctx context.Context, input *rds.ListTagsForResourceInput, accountID string) (*rds.ListTagsForResourceOutput, error) {
	return utils.NATSRequest[rds.ListTagsForResourceOutput](ctx, s.nc,
		SubjectListTagsForResource, input, defaultTimeout, accountID)
}

// Requested on the Layer-1 subject, not the bus.
func (s *NATSService) GetDBBootstrapConfig(ctx context.Context, input *GetDBBootstrapConfigInput, accountID string) (*GetDBBootstrapConfigOutput, error) {
	return utils.NATSRequest[GetDBBootstrapConfigOutput](ctx, s.nc,
		SubjectGetDBBootstrapConfig, input, defaultTimeout, accountID)
}
