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

// A lifecycle op waits on a graceful engine stop and then on the VM, both of
// which are bounded but neither of which fits the default budget. A delete adds
// the final snapshot and the teardown on top.
const (
	lifecycleTimeout = 4 * time.Minute
	deleteTimeout    = 6 * time.Minute
	// A class change terminates a VM and launches a replacement inline, so a
	// modify needs a create's budget rather than a lifecycle op's.
	modifyTimeout = 6 * time.Minute
)

// A snapshot create holds the engine quiesced while the COW snapshot is cut, and
// a delete may have to release the retained source volume behind it. A restore
// creates a volume from the snapshot and then launches a VM on it, so it costs a
// create plus the volume.
const (
	snapshotTimeout       = 6 * time.Minute
	snapshotDeleteTimeout = 4 * time.Minute
	restoreTimeout        = 8 * time.Minute
)

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

func (s *NATSService) RebootDBInstance(ctx context.Context, input *rds.RebootDBInstanceInput, accountID string) (*rds.RebootDBInstanceOutput, error) {
	return utils.NATSRequest[rds.RebootDBInstanceOutput](ctx, s.nc,
		SubjectRebootDBInstance, input, lifecycleTimeout, accountID)
}

func (s *NATSService) StartDBInstance(ctx context.Context, input *rds.StartDBInstanceInput, accountID string) (*rds.StartDBInstanceOutput, error) {
	return utils.NATSRequest[rds.StartDBInstanceOutput](ctx, s.nc,
		SubjectStartDBInstance, input, lifecycleTimeout, accountID)
}

func (s *NATSService) StopDBInstance(ctx context.Context, input *rds.StopDBInstanceInput, accountID string) (*rds.StopDBInstanceOutput, error) {
	return utils.NATSRequest[rds.StopDBInstanceOutput](ctx, s.nc,
		SubjectStopDBInstance, input, lifecycleTimeout, accountID)
}

func (s *NATSService) ModifyDBInstance(ctx context.Context, input *rds.ModifyDBInstanceInput, accountID string) (*rds.ModifyDBInstanceOutput, error) {
	return utils.NATSRequest[rds.ModifyDBInstanceOutput](ctx, s.nc,
		SubjectModifyDBInstance, input, modifyTimeout, accountID)
}

func (s *NATSService) DeleteDBInstance(ctx context.Context, input *rds.DeleteDBInstanceInput, accountID string) (*rds.DeleteDBInstanceOutput, error) {
	return utils.NATSRequest[rds.DeleteDBInstanceOutput](ctx, s.nc,
		SubjectDeleteDBInstance, input, deleteTimeout, accountID)
}

func (s *NATSService) CreateDBSnapshot(ctx context.Context, input *rds.CreateDBSnapshotInput, accountID string) (*rds.CreateDBSnapshotOutput, error) {
	return utils.NATSRequest[rds.CreateDBSnapshotOutput](ctx, s.nc,
		SubjectCreateDBSnapshot, input, snapshotTimeout, accountID)
}

func (s *NATSService) DescribeDBSnapshots(ctx context.Context, input *rds.DescribeDBSnapshotsInput, accountID string) (*rds.DescribeDBSnapshotsOutput, error) {
	return utils.NATSRequest[rds.DescribeDBSnapshotsOutput](ctx, s.nc,
		SubjectDescribeDBSnapshots, input, defaultTimeout, accountID)
}

func (s *NATSService) DeleteDBSnapshot(ctx context.Context, input *rds.DeleteDBSnapshotInput, accountID string) (*rds.DeleteDBSnapshotOutput, error) {
	return utils.NATSRequest[rds.DeleteDBSnapshotOutput](ctx, s.nc,
		SubjectDeleteDBSnapshot, input, snapshotDeleteTimeout, accountID)
}

func (s *NATSService) RestoreDBInstanceFromDBSnapshot(ctx context.Context, input *rds.RestoreDBInstanceFromDBSnapshotInput, accountID string) (*rds.RestoreDBInstanceFromDBSnapshotOutput, error) {
	return utils.NATSRequest[rds.RestoreDBInstanceFromDBSnapshotOutput](ctx, s.nc,
		SubjectRestoreDBInstanceFromDBSnapshot, input, restoreTimeout, accountID)
}

func (s *NATSService) DescribeDBInstanceAutomatedBackups(ctx context.Context,
	input *rds.DescribeDBInstanceAutomatedBackupsInput, accountID string) (*rds.DescribeDBInstanceAutomatedBackupsOutput, error) {
	return utils.NATSRequest[rds.DescribeDBInstanceAutomatedBackupsOutput](ctx, s.nc,
		SubjectDescribeDBInstanceAutomatedBackups, input, defaultTimeout, accountID)
}

func (s *NATSService) DescribeEvents(ctx context.Context, input *rds.DescribeEventsInput, accountID string) (*rds.DescribeEventsOutput, error) {
	return utils.NATSRequest[rds.DescribeEventsOutput](ctx, s.nc,
		SubjectDescribeEvents, input, defaultTimeout, accountID)
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

func (s *NATSService) CreateDBSubnetGroup(ctx context.Context, input *rds.CreateDBSubnetGroupInput, accountID string) (*rds.CreateDBSubnetGroupOutput, error) {
	return utils.NATSRequest[rds.CreateDBSubnetGroupOutput](ctx, s.nc,
		SubjectCreateDBSubnetGroup, input, defaultTimeout, accountID)
}

func (s *NATSService) DescribeDBSubnetGroups(ctx context.Context, input *rds.DescribeDBSubnetGroupsInput, accountID string) (*rds.DescribeDBSubnetGroupsOutput, error) {
	return utils.NATSRequest[rds.DescribeDBSubnetGroupsOutput](ctx, s.nc,
		SubjectDescribeDBSubnetGroups, input, defaultTimeout, accountID)
}

func (s *NATSService) DeleteDBSubnetGroup(ctx context.Context, input *rds.DeleteDBSubnetGroupInput, accountID string) (*rds.DeleteDBSubnetGroupOutput, error) {
	return utils.NATSRequest[rds.DeleteDBSubnetGroupOutput](ctx, s.nc,
		SubjectDeleteDBSubnetGroup, input, defaultTimeout, accountID)
}

func (s *NATSService) CreateDBParameterGroup(ctx context.Context, input *rds.CreateDBParameterGroupInput, accountID string) (*rds.CreateDBParameterGroupOutput, error) {
	return utils.NATSRequest[rds.CreateDBParameterGroupOutput](ctx, s.nc,
		SubjectCreateDBParameterGroup, input, defaultTimeout, accountID)
}

func (s *NATSService) DescribeDBParameterGroups(ctx context.Context, input *rds.DescribeDBParameterGroupsInput, accountID string) (*rds.DescribeDBParameterGroupsOutput, error) {
	return utils.NATSRequest[rds.DescribeDBParameterGroupsOutput](ctx, s.nc,
		SubjectDescribeDBParameterGroups, input, defaultTimeout, accountID)
}

func (s *NATSService) ModifyDBParameterGroup(ctx context.Context, input *rds.ModifyDBParameterGroupInput, accountID string) (*rds.DBParameterGroupNameMessage, error) {
	return utils.NATSRequest[rds.DBParameterGroupNameMessage](ctx, s.nc,
		SubjectModifyDBParameterGroup, input, defaultTimeout, accountID)
}

func (s *NATSService) DescribeDBParameters(ctx context.Context, input *rds.DescribeDBParametersInput, accountID string) (*rds.DescribeDBParametersOutput, error) {
	return utils.NATSRequest[rds.DescribeDBParametersOutput](ctx, s.nc,
		SubjectDescribeDBParameters, input, defaultTimeout, accountID)
}

func (s *NATSService) DeleteDBParameterGroup(ctx context.Context, input *rds.DeleteDBParameterGroupInput, accountID string) (*rds.DeleteDBParameterGroupOutput, error) {
	return utils.NATSRequest[rds.DeleteDBParameterGroupOutput](ctx, s.nc,
		SubjectDeleteDBParameterGroup, input, defaultTimeout, accountID)
}

// Requested on the Layer-1 subject, not the bus.
func (s *NATSService) GetDBBootstrapConfig(ctx context.Context, input *GetDBBootstrapConfigInput, accountID string) (*GetDBBootstrapConfigOutput, error) {
	return utils.NATSRequest[GetDBBootstrapConfigOutput](ctx, s.nc,
		SubjectGetDBBootstrapConfig, input, defaultTimeout, accountID)
}

func (s *NATSService) AcknowledgeDBBootstrap(ctx context.Context, input *AcknowledgeDBBootstrapInput, accountID string) (*AcknowledgeDBBootstrapOutput, error) {
	return utils.NATSRequest[AcknowledgeDBBootstrapOutput](ctx, s.nc,
		SubjectAcknowledgeDBBootstrap, input, defaultTimeout, accountID)
}
