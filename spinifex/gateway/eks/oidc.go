package gateway_eks

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/nats-io/nats.go"
)

// AssociateIdentityProviderConfig — POST /clusters/{name}/identity-provider-configs/associate
func AssociateIdentityProviderConfig(natsConn *nats.Conn, accountID, cluster string, body []byte) (*eks.AssociateIdentityProviderConfigOutput, error) {
	input := new(eks.AssociateIdentityProviderConfigInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	return handlers_eks.NewNATSEKSService(natsConn).AssociateIdentityProviderConfig(input, accountID)
}

// DescribeIdentityProviderConfig — POST /clusters/{name}/identity-provider-configs/describe
func DescribeIdentityProviderConfig(natsConn *nats.Conn, accountID, cluster string, body []byte) (*eks.DescribeIdentityProviderConfigOutput, error) {
	input := new(eks.DescribeIdentityProviderConfigInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	return handlers_eks.NewNATSEKSService(natsConn).DescribeIdentityProviderConfig(input, accountID)
}

// ListIdentityProviderConfigs — GET /clusters/{name}/identity-provider-configs
func ListIdentityProviderConfigs(natsConn *nats.Conn, accountID, cluster string) (*eks.ListIdentityProviderConfigsOutput, error) {
	input := &eks.ListIdentityProviderConfigsInput{ClusterName: aws.String(cluster)}
	return handlers_eks.NewNATSEKSService(natsConn).ListIdentityProviderConfigs(input, accountID)
}

// DisassociateIdentityProviderConfig — POST /clusters/{name}/identity-provider-configs/disassociate
func DisassociateIdentityProviderConfig(natsConn *nats.Conn, accountID, cluster string, body []byte) (*eks.DisassociateIdentityProviderConfigOutput, error) {
	input := new(eks.DisassociateIdentityProviderConfigInput)
	if err := unmarshalIfBody(body, input); err != nil {
		return nil, err
	}
	input.ClusterName = aws.String(cluster)
	return handlers_eks.NewNATSEKSService(natsConn).DisassociateIdentityProviderConfig(input, accountID)
}
