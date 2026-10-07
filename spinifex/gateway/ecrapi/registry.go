package gateway_ecrapi

import (
	"context"
	"encoding/json"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/nats-io/nats.go"
)

// DescribeRegistry reports the caller's registry. A deployment has no
// replication, so the configuration is always present with an empty rules
// list, which is what AWS returns for an account with none configured.
func DescribeRegistry(_ context.Context, _ *nats.Conn, accountID string, body []byte) (any, error) {
	if len(body) > 0 {
		var req struct{}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, MalformedBodyError()
		}
	}
	return &ecr.DescribeRegistryOutput{
		RegistryId: aws.String(accountID),
		ReplicationConfiguration: &ecr.ReplicationConfiguration{
			Rules: []*ecr.ReplicationRule{},
		},
	}, nil
}
