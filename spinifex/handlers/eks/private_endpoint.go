package handlers_eks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

// PrivateEndpointENI is the customer-VPC (Set A) ENI threaded onto the cluster
// NLB's LB VM to give in-VPC clients a private apiserver endpoint.
type PrivateEndpointENI struct {
	ENIID  string
	ENIIP  string
	ENIMac string
	SGID   string
}

// EnsurePrivateEndpointENI provisions the customer-VPC SG (admitting the customer
// VPC CIDR on :443) and a customer-account ENI in the first customer subnet. The
// ENI's private IP is the private apiserver endpoint: it is threaded onto the
// cluster NLB's LB VM as a cross-account extra NIC, so in-VPC workers + kubectl
// reach the control plane without the public hairpin / NAT GW egress. Created
// before the NLB + CP VMs so its IP is a known cert SAN.
func EnsurePrivateEndpointENI(ctx context.Context,
	vpcSvc k3sVPCProvisioner,
	sgp sgProvisioner,
	resolver SubnetVPCResolver,
	accountID, clusterName, subnetID, vpcID string,
) (*PrivateEndpointENI, error) {
	if accountID == "" {
		return nil, errors.New("eks: EnsurePrivateEndpointENI empty account id")
	}
	if subnetID == "" {
		return nil, errors.New("eks: EnsurePrivateEndpointENI empty subnet id")
	}
	if vpcID == "" {
		return nil, errors.New("eks: EnsurePrivateEndpointENI empty vpc id")
	}

	vpcCIDR, err := resolver.GetVPCCIDR(ctx, accountID, vpcID)
	if err != nil {
		return nil, fmt.Errorf("resolve customer VPC %s CIDR: %w", vpcID, err)
	}

	sgID, err := EnsurePrivateEndpointSG(ctx, sgp, accountID, clusterName, vpcID, vpcCIDR)
	if err != nil {
		return nil, err
	}

	eniOut, err := vpcSvc.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Description: aws.String("EKS private-endpoint ENI for " + clusterName),
		Groups:      aws.StringSlice([]string{sgID}),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("network-interface"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
				{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(clusterName)},
				{Key: aws.String(clusterEKSAccountTagKey), Value: aws.String(accountID)},
				{Key: aws.String(clusterEKSRoleTagKey), Value: aws.String(clusterEKSRolePrivateEndpoint)},
			},
		}},
	}, accountID)
	if err != nil {
		return nil, fmt.Errorf("create private-endpoint ENI in subnet %s: %w", subnetID, err)
	}
	if eniOut == nil || eniOut.NetworkInterface == nil ||
		aws.StringValue(eniOut.NetworkInterface.NetworkInterfaceId) == "" ||
		aws.StringValue(eniOut.NetworkInterface.PrivateIpAddress) == "" {
		return nil, errors.New("eks: CreateNetworkInterface returned incomplete private-endpoint ENI")
	}

	pe := &PrivateEndpointENI{
		ENIID:  aws.StringValue(eniOut.NetworkInterface.NetworkInterfaceId),
		ENIIP:  aws.StringValue(eniOut.NetworkInterface.PrivateIpAddress),
		ENIMac: aws.StringValue(eniOut.NetworkInterface.MacAddress),
		SGID:   sgID,
	}
	slog.InfoContext(ctx, "EnsurePrivateEndpointENI provisioned",
		"clusterName", clusterName, "accountID", accountID,
		"eniId", pe.ENIID, "eniIp", pe.ENIIP, "sgId", sgID)
	return pe, nil
}
