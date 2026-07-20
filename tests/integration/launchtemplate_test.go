//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// TestRunInstances_LaunchTemplateExpansion asserts the launch-template-facing
// slice of RunInstances: CreateLaunchTemplate returns a real template id and
// DefaultVersionNumber=1 (LaunchTemplateServiceImpl, wired via
// StartLaunchTemplateDaemonLite — real KV-backed logic, not a static stub),
// and RunInstances with only a LaunchTemplateSpecification resolves the
// template's ImageId/InstanceType and forwards them to the per-node launch —
// gateway/ec2/instance/RunInstances.go's expandLaunchTemplate, which calls
// handlers_ec2_launchtemplate.ExpandRunInstances over NATS before validation,
// routing, or placement ever see the request. That resolution is complete
// before the daemon-facing NATS hop, so a real guest is not needed to prove it
// happened — only that the per-node request actually carries the resolved
// fields rather than the direct RunInstancesInput ones (which are absent
// here: ImageId/InstanceType are supplied only via the template).
//
// The live test's EnsureDefaultVPC/RunInstances/WaitForInstanceState(running)/
// Terminate/WaitForInstanceState(terminated) sequence is left behind: once the
// per-node request is known to carry the resolved AMI and instance type, its
// only remaining job was waiting out a real guest boot to read them back off
// DescribeInstances — teardown hygiene for a real throwaway VM, not a
// distinct assertion. The template CRUD/versioning lifecycle (create version,
// modify default version, delete a version, tag-filtered describe) never
// touched a guest and is unaffected — it stays in
// tests/e2e/single/launchtemplate_test.go.
func TestRunInstances_LaunchTemplateExpansion(t *testing.T) {
	gw := StartGateway(t)
	StartLaunchTemplateDaemonLite(t, gw)

	const (
		amiID        = "ami-0123456789abcdef0"
		instanceType = "t3.micro"
		nodeID       = "node-1"
	)

	client := gw.EC2Client(t)

	createOut, err := client.CreateLaunchTemplate(&ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("integration-lt-run-instances"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			ImageId:      aws.String(amiID),
			InstanceType: aws.String(instanceType),
		},
	})
	require.NoError(t, err, "create-launch-template")
	require.NotNil(t, createOut.LaunchTemplate, "create-launch-template returned no template")
	templateID := aws.StringValue(createOut.LaunchTemplate.LaunchTemplateId)
	require.NotEmpty(t, templateID, "create-launch-template returned an empty id")
	require.Equal(t, int64(1), aws.Int64Value(createOut.LaunchTemplate.DefaultVersionNumber))

	// A single node reporting capacity for instanceType — enough for
	// distributeInstances to resolve MinCount=MaxCount=1 onto one node.
	statusResp := mustMarshal(t, &types.NodeStatusResponse{
		Node: nodeID,
		InstanceTypes: []types.InstanceTypeCap{
			{Name: instanceType, Available: 1},
		},
	})
	gw.StubSubject(t, "spinifex.node.status", statusResp)

	// Subscribe directly to the per-node launch subject (rather than
	// StubSubject's static responder) so the captured ImageId/InstanceType
	// prove the gateway forwarded the template-resolved values, not the
	// degenerate zero-daemon path (also InsufficientInstanceCapacity — see
	// placement.go:44) and not a stub that ignores its input.
	var gotImageID, gotInstanceType string
	launchSubject := fmt.Sprintf("ec2.RunInstances.%s.%s", instanceType, nodeID)
	sub, err := gw.NATSConn.Subscribe(launchSubject, func(msg *nats.Msg) {
		var nodeInput ec2.RunInstancesInput
		if jsonErr := json.Unmarshal(msg.Data, &nodeInput); jsonErr != nil {
			t.Errorf("unmarshal per-node RunInstances request: %v", jsonErr)
			return
		}
		gotImageID = aws.StringValue(nodeInput.ImageId)
		gotInstanceType = aws.StringValue(nodeInput.InstanceType)
		_ = msg.Respond(mustMarshal(t, &ec2.Reservation{
			ReservationId: aws.String("r-launch-template"),
			Instances: []*ec2.Instance{
				{InstanceId: aws.String("i-launch-template-1")},
			},
		}))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Deliberately no ImageId/InstanceType here — only the template reference.
	// If expandLaunchTemplate did not run, ValidateRunInstancesInput would
	// reject the request for a missing ImageId/InstanceType before ever
	// reaching the per-node subject.
	out, err := client.RunInstances(&ec2.RunInstancesInput{
		LaunchTemplate: &ec2.LaunchTemplateSpecification{LaunchTemplateId: aws.String(templateID)},
		MinCount:       aws.Int64(1),
		MaxCount:       aws.Int64(1),
	})
	require.NoError(t, err, "run-instances --launch-template")
	require.Lenf(t, out.Instances, 1, "expected 1 instance from run-instances, got %d", len(out.Instances))
	require.NotEmpty(t, aws.StringValue(out.Instances[0].InstanceId), "InstanceId empty")

	require.Equal(t, amiID, gotImageID, "gateway must resolve the template's ImageId onto the per-node launch")
	require.Equal(t, instanceType, gotInstanceType, "gateway must resolve the template's InstanceType onto the per-node launch")
}
