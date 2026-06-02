package gateway_eks

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// CreateAccessEntry — POST /clusters/{name}/access-entries
func CreateAccessEntry(natsConn *nats.Conn, accountID, cluster string, body []byte) (*eks.CreateAccessEntryOutput, error) {
	input := new(eks.CreateAccessEntryInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	return handlers_eks.NewNATSEKSService(natsConn).CreateAccessEntry(input, accountID)
}

// DescribeAccessEntry — GET /clusters/{name}/access-entries/{arn}
func DescribeAccessEntry(natsConn *nats.Conn, accountID, cluster, principalARN string) (*eks.DescribeAccessEntryOutput, error) {
	input := &eks.DescribeAccessEntryInput{
		ClusterName:  aws.String(cluster),
		PrincipalArn: aws.String(principalARN),
	}
	return handlers_eks.NewNATSEKSService(natsConn).DescribeAccessEntry(input, accountID)
}

// ListAccessEntries — GET /clusters/{name}/access-entries
func ListAccessEntries(natsConn *nats.Conn, accountID, cluster string) (*eks.ListAccessEntriesOutput, error) {
	input := &eks.ListAccessEntriesInput{ClusterName: aws.String(cluster)}
	return handlers_eks.NewNATSEKSService(natsConn).ListAccessEntries(input, accountID)
}

// UpdateAccessEntry — POST /clusters/{name}/access-entries/{arn}
func UpdateAccessEntry(natsConn *nats.Conn, accountID, cluster, principalARN string, body []byte) (*eks.UpdateAccessEntryOutput, error) {
	input := new(eks.UpdateAccessEntryInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	input.PrincipalArn = aws.String(principalARN)
	return handlers_eks.NewNATSEKSService(natsConn).UpdateAccessEntry(input, accountID)
}

// DeleteAccessEntry — DELETE /clusters/{name}/access-entries/{arn}
func DeleteAccessEntry(natsConn *nats.Conn, accountID, cluster, principalARN string) (*eks.DeleteAccessEntryOutput, error) {
	input := &eks.DeleteAccessEntryInput{
		ClusterName:  aws.String(cluster),
		PrincipalArn: aws.String(principalARN),
	}
	return handlers_eks.NewNATSEKSService(natsConn).DeleteAccessEntry(input, accountID)
}

// AssociateAccessPolicy — POST /clusters/{name}/access-entries/{arn}/access-policies
func AssociateAccessPolicy(natsConn *nats.Conn, accountID, cluster, principalARN string, body []byte) (*eks.AssociateAccessPolicyOutput, error) {
	input := new(eks.AssociateAccessPolicyInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	input.PrincipalArn = aws.String(principalARN)
	return handlers_eks.NewNATSEKSService(natsConn).AssociateAccessPolicy(input, accountID)
}

// DisassociateAccessPolicy — DELETE /clusters/{name}/access-entries/{arn}/access-policies
func DisassociateAccessPolicy(natsConn *nats.Conn, accountID, cluster, principalARN string, body []byte) (*eks.DisassociateAccessPolicyOutput, error) {
	input := new(eks.DisassociateAccessPolicyInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	input.PrincipalArn = aws.String(principalARN)
	return handlers_eks.NewNATSEKSService(natsConn).DisassociateAccessPolicy(input, accountID)
}

// ListAssociatedAccessPolicies — GET /clusters/{name}/access-entries/{arn}/access-policies
func ListAssociatedAccessPolicies(natsConn *nats.Conn, accountID, cluster, principalARN string) (*eks.ListAssociatedAccessPoliciesOutput, error) {
	input := &eks.ListAssociatedAccessPoliciesInput{
		ClusterName:  aws.String(cluster),
		PrincipalArn: aws.String(principalARN),
	}
	return handlers_eks.NewNATSEKSService(natsConn).ListAssociatedAccessPolicies(input, accountID)
}

// ListAccessPolicies — GET /access-policies
func ListAccessPolicies(natsConn *nats.Conn, accountID string) (*eks.ListAccessPoliciesOutput, error) {
	return handlers_eks.NewNATSEKSService(natsConn).ListAccessPolicies(&eks.ListAccessPoliciesInput{}, accountID)
}
