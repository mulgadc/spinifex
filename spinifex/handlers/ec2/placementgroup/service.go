package handlers_ec2_placementgroup

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// PlacementGroupService defines the interface for placement group operations.
type PlacementGroupService interface {
	CreatePlacementGroup(ctx context.Context, input *ec2.CreatePlacementGroupInput, accountID string) (*ec2.CreatePlacementGroupOutput, error)
	DeletePlacementGroup(ctx context.Context, input *ec2.DeletePlacementGroupInput, accountID string) (*ec2.DeletePlacementGroupOutput, error)
	DescribePlacementGroups(ctx context.Context, input *ec2.DescribePlacementGroupsInput, accountID string) (*ec2.DescribePlacementGroupsOutput, error)
	ReserveSpreadNodes(ctx context.Context, input *ReserveSpreadNodesInput, accountID string) (*ReserveSpreadNodesOutput, error)
	FinalizeSpreadInstances(ctx context.Context, input *FinalizeSpreadInstancesInput, accountID string) (*FinalizeSpreadInstancesOutput, error)
	ReleaseSpreadNodes(ctx context.Context, input *ReleaseSpreadNodesInput, accountID string) (*ReleaseSpreadNodesOutput, error)
	RemoveInstance(ctx context.Context, input *RemoveInstanceInput, accountID string) (*RemoveInstanceOutput, error)
	ReserveClusterNode(ctx context.Context, input *ReserveClusterNodeInput, accountID string) (*ReserveClusterNodeOutput, error)
	FinalizeClusterInstances(ctx context.Context, input *FinalizeClusterInstancesInput, accountID string) (*FinalizeClusterInstancesOutput, error)
}

// ReserveSpreadNodesInput requests atomic node reservation for a spread placement group.
type ReserveSpreadNodesInput struct {
	GroupName     string   `json:"group_name"`
	EligibleNodes []string `json:"eligible_nodes"` // nodes with capacity (from fan-out)
	MinCount      int      `json:"min_count"`
	MaxCount      int      `json:"max_count"`
}

// ReserveSpreadNodesOutput contains the nodes selected for launch.
type ReserveSpreadNodesOutput struct {
	ReservedNodes []string `json:"reserved_nodes"`
}

// FinalizeSpreadInstancesInput records actual instance IDs on reserved nodes.
type FinalizeSpreadInstancesInput struct {
	GroupName     string              `json:"group_name"`
	NodeInstances map[string][]string `json:"node_instances"` // node -> instance IDs
}

// FinalizeSpreadInstancesOutput is empty on success.
type FinalizeSpreadInstancesOutput struct{}

// ReleaseSpreadNodesInput releases previously reserved node slots (rollback).
type ReleaseSpreadNodesInput struct {
	GroupName string   `json:"group_name"`
	Nodes     []string `json:"nodes"`
}

// ReleaseSpreadNodesOutput is empty on success.
type ReleaseSpreadNodesOutput struct{}

// RemoveInstanceInput removes a specific instance from its placement group's NodeInstances.
type RemoveInstanceInput struct {
	GroupName  string `json:"group_name"`
	NodeName   string `json:"node_name"`
	InstanceID string `json:"instance_id"`
}

// RemoveInstanceOutput is empty on success.
type RemoveInstanceOutput struct{}

// ReserveClusterNodeInput requests target node determination for a cluster placement group.
type ReserveClusterNodeInput struct {
	GroupName     string   `json:"group_name"`
	EligibleNodes []string `json:"eligible_nodes"` // nodes with capacity, sorted by capacity desc
}

// ReserveClusterNodeOutput contains the target node for cluster placement.
type ReserveClusterNodeOutput struct {
	TargetNode string `json:"target_node"`
}

// FinalizeClusterInstancesInput records instance IDs launched in a cluster placement group.
type FinalizeClusterInstancesInput struct {
	GroupName     string              `json:"group_name"`
	NodeInstances map[string][]string `json:"node_instances"` // node -> instance IDs to append
}

// FinalizeClusterInstancesOutput is empty on success.
type FinalizeClusterInstancesOutput struct{}
