package gateway_eks

import (
	"context"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	"github.com/nats-io/nats.go"
)

// CreateAccessEntry — POST /clusters/{name}/access-entries.
func CreateAccessEntry(ctx context.Context, natsConn *nats.Conn, accountID, cluster string, body []byte) (*eks.CreateAccessEntryOutput, error) {
	input := new(eks.CreateAccessEntryInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	return ekscluster.NewNATSEKSService(natsConn).CreateAccessEntry(ctx, input, accountID)
}

// DescribeAccessEntry — GET /clusters/{name}/access-entries/{arn}.
func DescribeAccessEntry(ctx context.Context, natsConn *nats.Conn, accountID, cluster, principalARN string) (*eks.DescribeAccessEntryOutput, error) {
	input := &eks.DescribeAccessEntryInput{
		ClusterName:  aws.String(cluster),
		PrincipalArn: aws.String(principalARN),
	}
	return ekscluster.NewNATSEKSService(natsConn).DescribeAccessEntry(ctx, input, accountID)
}

// ListAccessEntries — GET /clusters/{name}/access-entries.
func ListAccessEntries(ctx context.Context, natsConn *nats.Conn, accountID, cluster string) (*eks.ListAccessEntriesOutput, error) {
	input := &eks.ListAccessEntriesInput{ClusterName: aws.String(cluster)}
	return ekscluster.NewNATSEKSService(natsConn).ListAccessEntries(ctx, input, accountID)
}

// UpdateAccessEntry — POST /clusters/{name}/access-entries/{arn}.
func UpdateAccessEntry(ctx context.Context, natsConn *nats.Conn, accountID, cluster, principalARN string, body []byte) (*eks.UpdateAccessEntryOutput, error) {
	input := new(eks.UpdateAccessEntryInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	input.PrincipalArn = aws.String(principalARN)
	return ekscluster.NewNATSEKSService(natsConn).UpdateAccessEntry(ctx, input, accountID)
}

// DeleteAccessEntry — DELETE /clusters/{name}/access-entries/{arn}.
func DeleteAccessEntry(ctx context.Context, natsConn *nats.Conn, accountID, cluster, principalARN string) (*eks.DeleteAccessEntryOutput, error) {
	input := &eks.DeleteAccessEntryInput{
		ClusterName:  aws.String(cluster),
		PrincipalArn: aws.String(principalARN),
	}
	return ekscluster.NewNATSEKSService(natsConn).DeleteAccessEntry(ctx, input, accountID)
}

// AssociateAccessPolicy — POST /clusters/{name}/access-entries/{arn}/access-policies.
func AssociateAccessPolicy(ctx context.Context, natsConn *nats.Conn, accountID, cluster, principalARN string, body []byte) (*eks.AssociateAccessPolicyOutput, error) {
	input := new(eks.AssociateAccessPolicyInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	input.PrincipalArn = aws.String(principalARN)
	return ekscluster.NewNATSEKSService(natsConn).AssociateAccessPolicy(ctx, input, accountID)
}

// DisassociateAccessPolicy — DELETE /clusters/{name}/access-entries/{arn}/access-policies/{policyArn}.
func DisassociateAccessPolicy(ctx context.Context, natsConn *nats.Conn, accountID, cluster, principalARN, policyARN string) (*eks.DisassociateAccessPolicyOutput, error) {
	input := &eks.DisassociateAccessPolicyInput{
		ClusterName:  aws.String(cluster),
		PrincipalArn: aws.String(principalARN),
		PolicyArn:    aws.String(policyARN),
	}
	return ekscluster.NewNATSEKSService(natsConn).DisassociateAccessPolicy(ctx, input, accountID)
}

// ListAssociatedAccessPolicies — GET /clusters/{name}/access-entries/{arn}/access-policies.
func ListAssociatedAccessPolicies(ctx context.Context, natsConn *nats.Conn, accountID, cluster, principalARN string) (*eks.ListAssociatedAccessPoliciesOutput, error) {
	input := &eks.ListAssociatedAccessPoliciesInput{
		ClusterName:  aws.String(cluster),
		PrincipalArn: aws.String(principalARN),
	}
	return ekscluster.NewNATSEKSService(natsConn).ListAssociatedAccessPolicies(ctx, input, accountID)
}

// ListAccessPolicies — GET /access-policies.
func ListAccessPolicies(ctx context.Context, natsConn *nats.Conn, accountID string) (*eks.ListAccessPoliciesOutput, error) {
	return ekscluster.NewNATSEKSService(natsConn).ListAccessPolicies(ctx, &eks.ListAccessPoliciesInput{}, accountID)
}
