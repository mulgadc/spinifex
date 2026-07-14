package handlers_ec2_placementgroup

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// NATSPlacementGroupService handles placement group operations via NATS messaging.
type NATSPlacementGroupService struct {
	natsConn *nats.Conn
}

var _ PlacementGroupService = (*NATSPlacementGroupService)(nil)

// NewNATSPlacementGroupService creates a new NATS-based placement group service.
func NewNATSPlacementGroupService(conn *nats.Conn) PlacementGroupService {
	return &NATSPlacementGroupService{natsConn: conn}
}

func (s *NATSPlacementGroupService) CreatePlacementGroup(ctx context.Context, input *ec2.CreatePlacementGroupInput, accountID string) (*ec2.CreatePlacementGroupOutput, error) {
	return utils.NATSRequest[ec2.CreatePlacementGroupOutput](ctx, s.natsConn, "ec2.CreatePlacementGroup", input, 30*time.Second, accountID)
}

func (s *NATSPlacementGroupService) DeletePlacementGroup(ctx context.Context, input *ec2.DeletePlacementGroupInput, accountID string) (*ec2.DeletePlacementGroupOutput, error) {
	return utils.NATSRequest[ec2.DeletePlacementGroupOutput](ctx, s.natsConn, "ec2.DeletePlacementGroup", input, 30*time.Second, accountID)
}

func (s *NATSPlacementGroupService) DescribePlacementGroups(ctx context.Context, input *ec2.DescribePlacementGroupsInput, accountID string) (*ec2.DescribePlacementGroupsOutput, error) {
	return utils.NATSRequest[ec2.DescribePlacementGroupsOutput](ctx, s.natsConn, "ec2.DescribePlacementGroups", input, 30*time.Second, accountID)
}

func (s *NATSPlacementGroupService) ReserveSpreadNodes(ctx context.Context, input *ReserveSpreadNodesInput, accountID string) (*ReserveSpreadNodesOutput, error) {
	return utils.NATSRequest[ReserveSpreadNodesOutput](ctx, s.natsConn, "ec2.ReserveSpreadNodes", input, 30*time.Second, accountID)
}

func (s *NATSPlacementGroupService) FinalizeSpreadInstances(ctx context.Context, input *FinalizeSpreadInstancesInput, accountID string) (*FinalizeSpreadInstancesOutput, error) {
	return utils.NATSRequest[FinalizeSpreadInstancesOutput](ctx, s.natsConn, "ec2.FinalizeSpreadInstances", input, 30*time.Second, accountID)
}

func (s *NATSPlacementGroupService) ReleaseSpreadNodes(ctx context.Context, input *ReleaseSpreadNodesInput, accountID string) (*ReleaseSpreadNodesOutput, error) {
	return utils.NATSRequest[ReleaseSpreadNodesOutput](ctx, s.natsConn, "ec2.ReleaseSpreadNodes", input, 30*time.Second, accountID)
}

func (s *NATSPlacementGroupService) RemoveInstance(ctx context.Context, input *RemoveInstanceInput, accountID string) (*RemoveInstanceOutput, error) {
	return utils.NATSRequest[RemoveInstanceOutput](ctx, s.natsConn, "ec2.RemoveInstanceFromPlacementGroup", input, 30*time.Second, accountID)
}

func (s *NATSPlacementGroupService) ReserveClusterNode(ctx context.Context, input *ReserveClusterNodeInput, accountID string) (*ReserveClusterNodeOutput, error) {
	return utils.NATSRequest[ReserveClusterNodeOutput](ctx, s.natsConn, "ec2.ReserveClusterNode", input, 30*time.Second, accountID)
}

func (s *NATSPlacementGroupService) FinalizeClusterInstances(ctx context.Context, input *FinalizeClusterInstancesInput, accountID string) (*FinalizeClusterInstancesOutput, error) {
	return utils.NATSRequest[FinalizeClusterInstancesOutput](ctx, s.natsConn, "ec2.FinalizeClusterInstances", input, 30*time.Second, accountID)
}
