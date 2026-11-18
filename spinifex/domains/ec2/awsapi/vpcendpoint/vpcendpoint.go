// Package vpcendpoint implements the EC2 VPC endpoint actions and the reads of
// the managed prefix list their routes name: it forwards each request over NATS.
package vpcendpoint

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2prefixlist "github.com/mulgadc/spinifex/spinifex/domains/ec2/prefixlist"
	ec2vpcendpoint "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcendpoint"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

func forward[In, Out any](input *In, call func(*In) (*Out, error)) (Out, error) {
	var output Out
	if input == nil {
		return output, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	result, err := call(input)
	if err != nil {
		return output, err
	}
	return *result, nil
}

// CreateVpcEndpoint forwards a create to the service scoped to accountID.
func CreateVpcEndpoint(ctx context.Context, input *ec2.CreateVpcEndpointInput, natsConn *nats.Conn, accountID string) (ec2.CreateVpcEndpointOutput, error) {
	return forward(input, func(in *ec2.CreateVpcEndpointInput) (*ec2.CreateVpcEndpointOutput, error) {
		return ec2vpcendpoint.NewNATSVpcEndpointService(natsConn).CreateVpcEndpoint(ctx, in, accountID)
	})
}

// ModifyVpcEndpoint forwards a modify to the service scoped to accountID.
func ModifyVpcEndpoint(ctx context.Context, input *ec2.ModifyVpcEndpointInput, natsConn *nats.Conn, accountID string) (ec2.ModifyVpcEndpointOutput, error) {
	return forward(input, func(in *ec2.ModifyVpcEndpointInput) (*ec2.ModifyVpcEndpointOutput, error) {
		return ec2vpcendpoint.NewNATSVpcEndpointService(natsConn).ModifyVpcEndpoint(ctx, in, accountID)
	})
}

// DeleteVpcEndpoints forwards a batch delete to the service scoped to accountID.
func DeleteVpcEndpoints(ctx context.Context, input *ec2.DeleteVpcEndpointsInput, natsConn *nats.Conn, accountID string) (ec2.DeleteVpcEndpointsOutput, error) {
	return forward(input, func(in *ec2.DeleteVpcEndpointsInput) (*ec2.DeleteVpcEndpointsOutput, error) {
		return ec2vpcendpoint.NewNATSVpcEndpointService(natsConn).DeleteVpcEndpoints(ctx, in, accountID)
	})
}

// DescribeVpcEndpoints forwards a describe to the service scoped to accountID.
func DescribeVpcEndpoints(ctx context.Context, input *ec2.DescribeVpcEndpointsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeVpcEndpointsOutput, error) {
	return forward(input, func(in *ec2.DescribeVpcEndpointsInput) (*ec2.DescribeVpcEndpointsOutput, error) {
		return ec2vpcendpoint.NewNATSVpcEndpointService(natsConn).DescribeVpcEndpoints(ctx, in, accountID)
	})
}

// DescribeVpcEndpointServices forwards a service listing to the service.
func DescribeVpcEndpointServices(ctx context.Context, input *ec2.DescribeVpcEndpointServicesInput, natsConn *nats.Conn, accountID string) (ec2.DescribeVpcEndpointServicesOutput, error) {
	return forward(input, func(in *ec2.DescribeVpcEndpointServicesInput) (*ec2.DescribeVpcEndpointServicesOutput, error) {
		return ec2vpcendpoint.NewNATSVpcEndpointService(natsConn).DescribeVpcEndpointServices(ctx, in, accountID)
	})
}

// DescribePrefixLists forwards a prefix-list read to the service.
func DescribePrefixLists(ctx context.Context, input *ec2.DescribePrefixListsInput, natsConn *nats.Conn, accountID string) (ec2.DescribePrefixListsOutput, error) {
	return forward(input, func(in *ec2.DescribePrefixListsInput) (*ec2.DescribePrefixListsOutput, error) {
		return ec2prefixlist.NewNATSPrefixListService(natsConn).DescribePrefixLists(ctx, in, accountID)
	})
}

// DescribeManagedPrefixLists forwards a managed prefix-list read to the service.
func DescribeManagedPrefixLists(ctx context.Context, input *ec2.DescribeManagedPrefixListsInput, natsConn *nats.Conn, accountID string) (ec2.DescribeManagedPrefixListsOutput, error) {
	return forward(input, func(in *ec2.DescribeManagedPrefixListsInput) (*ec2.DescribeManagedPrefixListsOutput, error) {
		return ec2prefixlist.NewNATSPrefixListService(natsConn).DescribeManagedPrefixLists(ctx, in, accountID)
	})
}

// GetManagedPrefixListEntries forwards an entries read to the service.
func GetManagedPrefixListEntries(ctx context.Context, input *ec2.GetManagedPrefixListEntriesInput, natsConn *nats.Conn, accountID string) (ec2.GetManagedPrefixListEntriesOutput, error) {
	return forward(input, func(in *ec2.GetManagedPrefixListEntriesInput) (*ec2.GetManagedPrefixListEntriesOutput, error) {
		return ec2prefixlist.NewNATSPrefixListService(natsConn).GetManagedPrefixListEntries(ctx, in, accountID)
	})
}
