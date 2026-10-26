package awsapi

import (
	"context"
	"encoding/json"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	"github.com/nats-io/nats.go"
)

// CreateCluster — POST /clusters. callerPrincipalARN is the resolved IAM
// principal, used to mint the bootstrap-creator-admin AccessEntry.
func CreateCluster(ctx context.Context, natsConn *nats.Conn, accountID, callerPrincipalARN string, body []byte) (*eks.CreateClusterOutput, error) {
	input := new(eks.CreateClusterInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	return ekscluster.NewNATSEKSService(natsConn).CreateCluster(ctx, input, accountID, callerPrincipalARN)
}

// DescribeCluster — GET /clusters/{name}.
func DescribeCluster(ctx context.Context, natsConn *nats.Conn, accountID, name string) (*eks.DescribeClusterOutput, error) {
	input := &eks.DescribeClusterInput{Name: aws.String(name)}
	return ekscluster.NewNATSEKSService(natsConn).DescribeCluster(ctx, input, accountID)
}

// ListClusters — GET /clusters.
func ListClusters(ctx context.Context, natsConn *nats.Conn, accountID string) (*eks.ListClustersOutput, error) {
	return ekscluster.NewNATSEKSService(natsConn).ListClusters(ctx, &eks.ListClustersInput{}, accountID)
}

// DeleteCluster — DELETE /clusters/{name}.
func DeleteCluster(ctx context.Context, natsConn *nats.Conn, accountID, name string) (*eks.DeleteClusterOutput, error) {
	input := &eks.DeleteClusterInput{Name: aws.String(name)}
	return ekscluster.NewNATSEKSService(natsConn).DeleteCluster(ctx, input, accountID)
}

// unmarshalIfBody decodes body into out only when non-empty.
func unmarshalIfBody(body []byte, out any) error {
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}
