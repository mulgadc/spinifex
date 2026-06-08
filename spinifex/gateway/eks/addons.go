package gateway_eks

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// ListAddons — GET /clusters/{name}/addons
func ListAddons(natsConn *nats.Conn, accountID, cluster string) (*eks.ListAddonsOutput, error) {
	return handlers_eks.NewNATSEKSService(natsConn).ListAddons(&eks.ListAddonsInput{
		ClusterName: aws.String(cluster),
	}, accountID)
}

// DescribeAddonVersions — GET /addons/supported-versions
func DescribeAddonVersions(natsConn *nats.Conn, accountID string) (*eks.DescribeAddonVersionsOutput, error) {
	return handlers_eks.NewNATSEKSService(natsConn).DescribeAddonVersions(&eks.DescribeAddonVersionsInput{}, accountID)
}

// CreateAddon — POST /clusters/{name}/addons
func CreateAddon(natsConn *nats.Conn, accountID, cluster string, body []byte) (*eks.CreateAddonOutput, error) {
	input := new(eks.CreateAddonInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	return handlers_eks.NewNATSEKSService(natsConn).CreateAddon(input, accountID)
}

// DeleteAddon — DELETE /clusters/{name}/addons/{addon}
func DeleteAddon(natsConn *nats.Conn, accountID, cluster, addon string) (*eks.DeleteAddonOutput, error) {
	input := &eks.DeleteAddonInput{
		ClusterName: aws.String(cluster),
		AddonName:   aws.String(addon),
	}
	return handlers_eks.NewNATSEKSService(natsConn).DeleteAddon(input, accountID)
}

// DescribeAddon — GET /clusters/{name}/addons/{addon}
func DescribeAddon(natsConn *nats.Conn, accountID, cluster, addon string) (*eks.DescribeAddonOutput, error) {
	input := &eks.DescribeAddonInput{
		ClusterName: aws.String(cluster),
		AddonName:   aws.String(addon),
	}
	return handlers_eks.NewNATSEKSService(natsConn).DescribeAddon(input, accountID)
}

// UpdateAddon — POST /clusters/{name}/addons/{addon}/update
func UpdateAddon(natsConn *nats.Conn, accountID, cluster, addon string, body []byte) (*eks.UpdateAddonOutput, error) {
	input := new(eks.UpdateAddonInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	input.AddonName = aws.String(addon)
	return handlers_eks.NewNATSEKSService(natsConn).UpdateAddon(input, accountID)
}
