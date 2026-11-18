package ecs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveWithCaller answers subject the way the daemon does, handing the
// request's principal header to fn through WithCaller.
func serveWithCaller[I, O any](t *testing.T, nc *nats.Conn, subject string, fn func(context.Context, *I, string) (*O, error)) {
	t.Helper()
	handler := WithCaller(fn)
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		in := new(I)
		if err := json.Unmarshal(msg.Data, in); err != nil {
			_ = msg.Respond(nil)
			return
		}
		out, err := handler(context.Background(), in, natsmsg.AccountIDFromMsg(msg), natsmsg.PrincipalARNFromMsg(msg))
		if err != nil {
			_ = msg.Respond([]byte(`{}`))
			return
		}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// AWS records the creating IAM role as a service's createdBy and the creating
// session as a task definition's registeredBy, and returns both on describe.
func TestCallerARN_RecordedAsCreatedByAndRegisteredBy(t *testing.T) {
	svc, nc := newTestService(t)
	serveWithCaller(t, nc, "ecs.RegisterTaskDefinition", svc.RegisterTaskDefinition)
	serveWithCaller(t, nc, "ecs.CreateService", svc.CreateService)
	_, err := svc.CreateCluster(t.Context(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)

	const roleARN = "arn:aws:iam::" + testAccountID + ":role/ops/deployer"
	const sessionARN = "arn:aws:sts::" + testAccountID + ":assumed-role/deployer/alice"
	gw := NewNATSECSService(nc)

	reg, err := gw.RegisterTaskDefinition(WithCallerARN(t.Context(), sessionARN), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Memory: aws.Int64(128),
		}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, sessionARN, aws.StringValue(reg.TaskDefinition.RegisteredBy))

	created, err := gw.CreateService(WithCallerARN(t.Context(), roleARN), &ecs.CreateServiceInput{
		Cluster: aws.String("web"), ServiceName: aws.String("web"), TaskDefinition: aws.String("app"), DesiredCount: aws.Int64(0),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, roleARN, aws.StringValue(created.Service.CreatedBy))

	td, err := svc.DescribeTaskDefinition(t.Context(), &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("app:1")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, sessionARN, aws.StringValue(td.TaskDefinition.RegisteredBy))

	described, err := svc.DescribeServices(t.Context(), &ecs.DescribeServicesInput{
		Cluster: aws.String("web"), Services: aws.StringSlice([]string{"web"}),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, described.Services, 1)
	assert.Equal(t, roleARN, aws.StringValue(described.Services[0].CreatedBy))
}
