package gateway_eks

import (
	"encoding/json"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// CreateCluster — POST /clusters
func CreateCluster(natsConn *nats.Conn, accountID string, body []byte) (*eks.CreateClusterOutput, error) {
	input := new(eks.CreateClusterInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_eks.NewNATSEKSService(natsConn).CreateCluster(input, accountID)
}

// DescribeCluster — GET /clusters/{name}
func DescribeCluster(natsConn *nats.Conn, accountID, name string) (*eks.DescribeClusterOutput, error) {
	input := &eks.DescribeClusterInput{Name: aws.String(name)}
	return handlers_eks.NewNATSEKSService(natsConn).DescribeCluster(input, accountID)
}

// ListClusters — GET /clusters
func ListClusters(natsConn *nats.Conn, accountID string) (*eks.ListClustersOutput, error) {
	return handlers_eks.NewNATSEKSService(natsConn).ListClusters(&eks.ListClustersInput{}, accountID)
}

// UpdateClusterConfig — POST /clusters/{name}/update-config
func UpdateClusterConfig(natsConn *nats.Conn, accountID, name string, body []byte) (*eks.UpdateClusterConfigOutput, error) {
	input := new(eks.UpdateClusterConfigInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.Name = aws.String(name)
	return handlers_eks.NewNATSEKSService(natsConn).UpdateClusterConfig(input, accountID)
}

// UpdateClusterVersion — POST /clusters/{name}/update-version
func UpdateClusterVersion(natsConn *nats.Conn, accountID, name string, body []byte) (*eks.UpdateClusterVersionOutput, error) {
	input := new(eks.UpdateClusterVersionInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.Name = aws.String(name)
	return handlers_eks.NewNATSEKSService(natsConn).UpdateClusterVersion(input, accountID)
}

// DeleteCluster — DELETE /clusters/{name}
func DeleteCluster(natsConn *nats.Conn, accountID, name string) (*eks.DeleteClusterOutput, error) {
	input := &eks.DeleteClusterInput{Name: aws.String(name)}
	return handlers_eks.NewNATSEKSService(natsConn).DeleteCluster(input, accountID)
}

// unmarshalIfBody decodes body into out only when body is non-empty.
// Returning a typed empty input on empty body lets us share a single helper
// across actions that may or may not carry a body.
func unmarshalIfBody(body []byte, out any) error {
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}
