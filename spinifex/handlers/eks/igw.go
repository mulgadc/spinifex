package handlers_eks

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

// igwProvisioner is the narrow Internet Gateway surface the cluster IGW helpers
// need. An internet-facing cluster endpoint allocates an external front-end IP
// and an OVN dnat_and_snat for it, but the external switch + localnet + gateway
// LRP that make that IP answerable on the physical wire are built only when an
// IGW is attached to the VPC. The control-plane NLB is a system instance — no
// customer provisions the VPC's IGW for it — so EKS ensures one itself. The
// daemon adapts the concrete IGW service onto this.
type igwProvisioner interface {
	DescribeInternetGateways(input *ec2.DescribeInternetGatewaysInput, accountID string) (*ec2.DescribeInternetGatewaysOutput, error)
	CreateInternetGateway(input *ec2.CreateInternetGatewayInput, accountID string) (*ec2.CreateInternetGatewayOutput, error)
	AttachInternetGateway(input *ec2.AttachInternetGatewayInput, accountID string) (*ec2.AttachInternetGatewayOutput, error)
	DetachInternetGateway(input *ec2.DetachInternetGatewayInput, accountID string) (*ec2.DetachInternetGatewayOutput, error)
	DeleteInternetGateway(input *ec2.DeleteInternetGatewayInput, accountID string) (*ec2.DeleteInternetGatewayOutput, error)
}

// EnsureClusterIGW guarantees vpcID has an attached Internet Gateway so an
// internet-facing cluster endpoint is reachable on the external network.
// Idempotent and non-destructive: if the VPC already has an attached IGW
// (customer-provisioned or from a prior launch) it is reused as-is and left
// untagged; only when none exists is a cluster-owned IGW created and attached.
func EnsureClusterIGW(igwp igwProvisioner, accountID, vpcID, clusterName string) error {
	if vpcID == "" {
		return errors.New("eks: EnsureClusterIGW empty vpc id")
	}
	if clusterName == "" {
		return errors.New("eks: EnsureClusterIGW empty cluster name")
	}

	existing, err := attachedVPCIGW(igwp, accountID, vpcID)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}

	out, err := igwp.CreateInternetGateway(&ec2.CreateInternetGatewayInput{
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("internet-gateway"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
				{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(clusterName)},
			},
		}},
	}, accountID)
	if err != nil {
		return fmt.Errorf("eks: create cluster IGW for vpc %s: %w", vpcID, err)
	}
	if out == nil || out.InternetGateway == nil || aws.StringValue(out.InternetGateway.InternetGatewayId) == "" {
		return fmt.Errorf("eks: create cluster IGW for vpc %s: empty gateway id", vpcID)
	}
	igwID := aws.StringValue(out.InternetGateway.InternetGatewayId)

	if _, err := igwp.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	}, accountID); err != nil {
		return fmt.Errorf("eks: attach cluster IGW %s to vpc %s: %w", igwID, vpcID, err)
	}
	slog.Info("EnsureClusterIGW: attached cluster IGW", "igw", igwID, "vpc", vpcID, "cluster", clusterName)
	return nil
}

// DeleteClusterIGW detaches and deletes the cluster-owned IGW attached to vpcID.
// Best-effort and ownership-scoped: it only removes an IGW that carries this
// cluster's managed-by tag, so a customer-provisioned IGW reused by
// EnsureClusterIGW is never deleted.
func DeleteClusterIGW(igwp igwProvisioner, accountID, vpcID, clusterName string) error {
	if vpcID == "" || clusterName == "" {
		return errors.New("eks: DeleteClusterIGW empty vpc id or cluster name")
	}

	igw, err := attachedVPCIGW(igwp, accountID, vpcID)
	if err != nil {
		slog.Warn("DeleteClusterIGW: IGW lookup failed", "vpc", vpcID, "err", err)
		return nil
	}
	if igw == nil || !ownedByCluster(igw, clusterName) {
		return nil
	}
	igwID := aws.StringValue(igw.InternetGatewayId)

	if _, err := igwp.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	}, accountID); err != nil {
		slog.Warn("DeleteClusterIGW: detach failed", "igw", igwID, "vpc", vpcID, "err", err)
		return nil
	}
	if _, err := igwp.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
	}, accountID); err != nil {
		slog.Warn("DeleteClusterIGW: delete failed", "igw", igwID, "err", err)
		return nil
	}
	slog.Info("DeleteClusterIGW: removed cluster IGW", "igw", igwID, "vpc", vpcID, "cluster", clusterName)
	return nil
}

// attachedVPCIGW returns the Internet Gateway attached to vpcID, or nil if none.
func attachedVPCIGW(igwp igwProvisioner, accountID, vpcID string) (*ec2.InternetGateway, error) {
	out, err := igwp.DescribeInternetGateways(&ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("attachment.vpc-id"),
			Values: aws.StringSlice([]string{vpcID}),
		}},
	}, accountID)
	if err != nil {
		return nil, fmt.Errorf("eks: describe IGWs for vpc %s: %w", vpcID, err)
	}
	if out == nil || len(out.InternetGateways) == 0 {
		return nil, nil
	}
	return out.InternetGateways[0], nil
}

// ownedByCluster reports whether igw carries this cluster's EKS managed-by tags.
func ownedByCluster(igw *ec2.InternetGateway, clusterName string) bool {
	var managed, named bool
	for _, t := range igw.Tags {
		switch aws.StringValue(t.Key) {
		case tags.ManagedByKey:
			managed = aws.StringValue(t.Value) == tags.ManagedByEKS
		case clusterEKSClusterTagKey:
			named = aws.StringValue(t.Value) == clusterName
		}
	}
	return managed && named
}
