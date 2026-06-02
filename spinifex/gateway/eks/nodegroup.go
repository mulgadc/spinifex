package gateway_eks

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// CreateNodegroup — POST /clusters/{name}/node-groups
func CreateNodegroup(natsConn *nats.Conn, accountID, cluster string, body []byte) (*eks.CreateNodegroupOutput, error) {
	input := new(eks.CreateNodegroupInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	return handlers_eks.NewNATSEKSService(natsConn).CreateNodegroup(input, accountID)
}

// DescribeNodegroup — GET /clusters/{name}/node-groups/{ng}
func DescribeNodegroup(natsConn *nats.Conn, accountID, cluster, ng string) (*eks.DescribeNodegroupOutput, error) {
	input := &eks.DescribeNodegroupInput{
		ClusterName:   aws.String(cluster),
		NodegroupName: aws.String(ng),
	}
	return handlers_eks.NewNATSEKSService(natsConn).DescribeNodegroup(input, accountID)
}

// ListNodegroups — GET /clusters/{name}/node-groups
func ListNodegroups(natsConn *nats.Conn, accountID, cluster string) (*eks.ListNodegroupsOutput, error) {
	input := &eks.ListNodegroupsInput{ClusterName: aws.String(cluster)}
	return handlers_eks.NewNATSEKSService(natsConn).ListNodegroups(input, accountID)
}

// UpdateNodegroupConfig — POST /clusters/{name}/node-groups/{ng}/update-config
func UpdateNodegroupConfig(natsConn *nats.Conn, accountID, cluster, ng string, body []byte) (*eks.UpdateNodegroupConfigOutput, error) {
	input := new(eks.UpdateNodegroupConfigInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	input.NodegroupName = aws.String(ng)
	return handlers_eks.NewNATSEKSService(natsConn).UpdateNodegroupConfig(input, accountID)
}

// UpdateNodegroupVersion — POST /clusters/{name}/node-groups/{ng}/update-version
func UpdateNodegroupVersion(natsConn *nats.Conn, accountID, cluster, ng string, body []byte) (*eks.UpdateNodegroupVersionOutput, error) {
	input := new(eks.UpdateNodegroupVersionInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	input.NodegroupName = aws.String(ng)
	return handlers_eks.NewNATSEKSService(natsConn).UpdateNodegroupVersion(input, accountID)
}

// DeleteNodegroup — DELETE /clusters/{name}/node-groups/{ng}
func DeleteNodegroup(natsConn *nats.Conn, accountID, cluster, ng string) (*eks.DeleteNodegroupOutput, error) {
	input := &eks.DeleteNodegroupInput{
		ClusterName:   aws.String(cluster),
		NodegroupName: aws.String(ng),
	}
	return handlers_eks.NewNATSEKSService(natsConn).DeleteNodegroup(input, accountID)
}
