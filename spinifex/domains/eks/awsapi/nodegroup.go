package awsapi

import (
	"context"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	"github.com/nats-io/nats.go"
)

// CreateNodegroup — POST /clusters/{name}/node-groups.
func CreateNodegroup(ctx context.Context, natsConn *nats.Conn, accountID, cluster string, body []byte) (*eks.CreateNodegroupOutput, error) {
	input := new(eks.CreateNodegroupInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	return ekscluster.NewNATSEKSService(natsConn).CreateNodegroup(ctx, input, accountID)
}

// DescribeNodegroup — GET /clusters/{name}/node-groups/{ng}.
func DescribeNodegroup(ctx context.Context, natsConn *nats.Conn, accountID, cluster, ng string) (*eks.DescribeNodegroupOutput, error) {
	input := &eks.DescribeNodegroupInput{
		ClusterName:   aws.String(cluster),
		NodegroupName: aws.String(ng),
	}
	return ekscluster.NewNATSEKSService(natsConn).DescribeNodegroup(ctx, input, accountID)
}

// ListNodegroups — GET /clusters/{name}/node-groups.
func ListNodegroups(ctx context.Context, natsConn *nats.Conn, accountID, cluster string) (*eks.ListNodegroupsOutput, error) {
	input := &eks.ListNodegroupsInput{ClusterName: aws.String(cluster)}
	return ekscluster.NewNATSEKSService(natsConn).ListNodegroups(ctx, input, accountID)
}

// UpdateNodegroupConfig — POST /clusters/{name}/node-groups/{ng}/update-config.
func UpdateNodegroupConfig(ctx context.Context, natsConn *nats.Conn, accountID, cluster, ng string, body []byte) (*eks.UpdateNodegroupConfigOutput, error) {
	input := new(eks.UpdateNodegroupConfigInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	input.NodegroupName = aws.String(ng)
	return ekscluster.NewNATSEKSService(natsConn).UpdateNodegroupConfig(ctx, input, accountID)
}

// DeleteNodegroup — DELETE /clusters/{name}/node-groups/{ng}.
func DeleteNodegroup(ctx context.Context, natsConn *nats.Conn, accountID, cluster, ng string) (*eks.DeleteNodegroupOutput, error) {
	input := &eks.DeleteNodegroupInput{
		ClusterName:   aws.String(cluster),
		NodegroupName: aws.String(ng),
	}
	return ekscluster.NewNATSEKSService(natsConn).DeleteNodegroup(ctx, input, accountID)
}
