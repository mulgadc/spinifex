package awsapi

import (
	"context"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	"github.com/nats-io/nats.go"
)

// ListAddons — GET /clusters/{name}/addons.
func ListAddons(ctx context.Context, natsConn *nats.Conn, accountID, cluster string) (*eks.ListAddonsOutput, error) {
	return ekscluster.NewNATSEKSService(natsConn).ListAddons(ctx, &eks.ListAddonsInput{
		ClusterName: aws.String(cluster),
	}, accountID)
}

// DescribeAddonVersions — GET /addons/supported-versions.
func DescribeAddonVersions(ctx context.Context, natsConn *nats.Conn, accountID string, query []byte) (*eks.DescribeAddonVersionsOutput, error) {
	q, err := parseListQuery(query)
	if err != nil {
		return nil, err
	}
	input := &eks.DescribeAddonVersionsInput{
		AddonName:         q.str("addonName"),
		KubernetesVersion: q.str("kubernetesVersion"),
		NextToken:         q.str("nextToken"),
		Types:             q.strs("types"),
		Publishers:        q.strs("publishers"),
		Owners:            q.strs("owners"),
	}
	if input.MaxResults, err = q.int64("maxResults"); err != nil {
		return nil, err
	}
	return ekscluster.NewNATSEKSService(natsConn).DescribeAddonVersions(ctx, input, accountID)
}

// CreateAddon — POST /clusters/{name}/addons.
func CreateAddon(ctx context.Context, natsConn *nats.Conn, accountID, cluster string, body []byte) (*eks.CreateAddonOutput, error) {
	input := new(eks.CreateAddonInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	return ekscluster.NewNATSEKSService(natsConn).CreateAddon(ctx, input, accountID)
}

// DeleteAddon — DELETE /clusters/{name}/addons/{addon}.
func DeleteAddon(ctx context.Context, natsConn *nats.Conn, accountID, cluster, addon string) (*eks.DeleteAddonOutput, error) {
	input := &eks.DeleteAddonInput{
		ClusterName: aws.String(cluster),
		AddonName:   aws.String(addon),
	}
	return ekscluster.NewNATSEKSService(natsConn).DeleteAddon(ctx, input, accountID)
}

// DescribeAddon — GET /clusters/{name}/addons/{addon}.
func DescribeAddon(ctx context.Context, natsConn *nats.Conn, accountID, cluster, addon string) (*eks.DescribeAddonOutput, error) {
	input := &eks.DescribeAddonInput{
		ClusterName: aws.String(cluster),
		AddonName:   aws.String(addon),
	}
	return ekscluster.NewNATSEKSService(natsConn).DescribeAddon(ctx, input, accountID)
}

// UpdateAddon — POST /clusters/{name}/addons/{addon}/update.
func UpdateAddon(ctx context.Context, natsConn *nats.Conn, accountID, cluster, addon string, body []byte) (*eks.UpdateAddonOutput, error) {
	input := new(eks.UpdateAddonInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	input.AddonName = aws.String(addon)
	return ekscluster.NewNATSEKSService(natsConn).UpdateAddon(ctx, input, accountID)
}
