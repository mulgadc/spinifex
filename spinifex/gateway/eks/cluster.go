package gateway_eks

import (
	"context"
	"encoding/json"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// CreateCluster — POST /clusters. callerPrincipalARN is the resolved IAM
// principal, used to mint the bootstrap-creator-admin AccessEntry.
func CreateCluster(ctx context.Context, natsConn *nats.Conn, accountID, callerPrincipalARN string, body []byte) (*eks.CreateClusterOutput, error) {
	input := new(eks.CreateClusterInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return handlers_eks.NewNATSEKSService(natsConn).CreateCluster(ctx, input, accountID, callerPrincipalARN)
}

// DescribeCluster — GET /clusters/{name}
func DescribeCluster(ctx context.Context, natsConn *nats.Conn, accountID, name string) (*eks.DescribeClusterOutput, error) {
	input := &eks.DescribeClusterInput{Name: aws.String(name)}
	return handlers_eks.NewNATSEKSService(natsConn).DescribeCluster(ctx, input, accountID)
}

// ListClusters — GET /clusters
func ListClusters(ctx context.Context, natsConn *nats.Conn, accountID string) (*eks.ListClustersOutput, error) {
	return handlers_eks.NewNATSEKSService(natsConn).ListClusters(ctx, &eks.ListClustersInput{}, accountID)
}

// UpdateClusterConfig — POST /clusters/{name}/update-config
func UpdateClusterConfig(ctx context.Context, natsConn *nats.Conn, accountID, name string, body []byte) (*eks.UpdateClusterConfigOutput, error) {
	input := new(eks.UpdateClusterConfigInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.Name = aws.String(name)
	return handlers_eks.NewNATSEKSService(natsConn).UpdateClusterConfig(ctx, input, accountID)
}

// UpdateClusterVersion — POST /clusters/{name}/update-version
func UpdateClusterVersion(ctx context.Context, natsConn *nats.Conn, accountID, name string, body []byte) (*eks.UpdateClusterVersionOutput, error) {
	input := new(eks.UpdateClusterVersionInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.Name = aws.String(name)
	return handlers_eks.NewNATSEKSService(natsConn).UpdateClusterVersion(ctx, input, accountID)
}

// DeleteCluster — DELETE /clusters/{name}
func DeleteCluster(ctx context.Context, natsConn *nats.Conn, accountID, name string) (*eks.DeleteClusterOutput, error) {
	input := &eks.DeleteClusterInput{Name: aws.String(name)}
	return handlers_eks.NewNATSEKSService(natsConn).DeleteCluster(ctx, input, accountID)
}

// unmarshalIfBody decodes body into out only when non-empty.
func unmarshalIfBody(body []byte, out any) error {
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}
