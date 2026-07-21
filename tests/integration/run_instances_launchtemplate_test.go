//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// captureLaunchTemplateNodeInput wires a per-node launch responder that
// forwards each decoded RunInstancesInput onto the returned channel before
// replying, so a test can inspect exactly what expandLaunchTemplate
// (handlers/ec2/launchtemplate/expand.go, called from RunInstances.go) merged
// onto the request the gateway actually dispatched.
func captureLaunchTemplateNodeInput(t *testing.T, gw *Gateway, instanceType, nodeID string) <-chan *ec2.RunInstancesInput {
	t.Helper()
	ch := make(chan *ec2.RunInstancesInput, 4)
	subject := fmt.Sprintf("ec2.RunInstances.%s.%s", instanceType, nodeID)
	sub, err := gw.NATSConn.Subscribe(subject, func(msg *nats.Msg) {
		var nodeInput ec2.RunInstancesInput
		if err := json.Unmarshal(msg.Data, &nodeInput); err != nil {
			t.Errorf("unmarshal per-node RunInstances request: %v", err)
			return
		}
		ch <- &nodeInput
		_ = msg.Respond(mustMarshal(t, &ec2.Reservation{
			ReservationId: aws.String("r-lt"),
			Instances:     []*ec2.Instance{{InstanceId: aws.String("i-lt")}},
		}))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return ch
}

// awaitLaunchTemplateNodeInput reads one captured RunInstancesInput or fails
// the test, so a hung dispatch (e.g. expansion never reaching the per-node
// subject) surfaces as a fast, clear failure instead of the SDK's own timeout.
func awaitLaunchTemplateNodeInput(t *testing.T, ch <-chan *ec2.RunInstancesInput) *ec2.RunInstancesInput {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for per-node RunInstances dispatch")
		return nil
	}
}

// TestRunInstances_LaunchTemplateVersionSelection proves ExpandRunInstances
// resolves the referenced launch template version before validation: an
// omitted Version resolves to the template's default (version 1, set at
// creation), while an explicit Version selects that specific version's data.
func TestRunInstances_LaunchTemplateVersionSelection(t *testing.T) {
	gw := StartGateway(t)
	StartLaunchTemplateDaemonLite(t, gw)

	const (
		templateName = "lt-version-selection"
		instanceType = "t3.micro"
		nodeID       = "lt-node"
		amiV1        = "ami-0000000000000001"
		amiV2        = "ami-0000000000000002"
	)

	gw.StubSubject(t, "spinifex.node.status", mustMarshal(t, &types.NodeStatusResponse{
		Node:          nodeID,
		InstanceTypes: []types.InstanceTypeCap{{Name: instanceType, Available: 5}},
	}))
	nodeCh := captureLaunchTemplateNodeInput(t, gw, instanceType, nodeID)

	_, err := gw.EC2Client(t).CreateLaunchTemplate(&ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(templateName),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			ImageId:      aws.String(amiV1),
			InstanceType: aws.String(instanceType),
			KeyName:      aws.String("template-key-v1"),
		},
	})
	require.NoError(t, err, "create-launch-template")

	_, err = gw.EC2Client(t).CreateLaunchTemplateVersion(&ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateName: aws.String(templateName),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			ImageId:      aws.String(amiV2),
			InstanceType: aws.String(instanceType),
			KeyName:      aws.String("template-key-v2"),
		},
	})
	require.NoError(t, err, "create-launch-template-version 2")

	// Version omitted -> $Default, which CreateLaunchTemplateVersion never
	// moved off version 1.
	_, err = gw.EC2Client(t).RunInstances(&ec2.RunInstancesInput{
		MinCount: aws.Int64(1),
		MaxCount: aws.Int64(1),
		LaunchTemplate: &ec2.LaunchTemplateSpecification{
			LaunchTemplateName: aws.String(templateName),
		},
	})
	require.NoError(t, err, "run-instances with default launch-template version")
	gotDefault := awaitLaunchTemplateNodeInput(t, nodeCh)
	require.Equal(t, amiV1, aws.StringValue(gotDefault.ImageId), "omitted Version must resolve to the template default (v1)")

	// Explicit Version="2" must resolve the second version's data instead.
	_, err = gw.EC2Client(t).RunInstances(&ec2.RunInstancesInput{
		MinCount: aws.Int64(1),
		MaxCount: aws.Int64(1),
		LaunchTemplate: &ec2.LaunchTemplateSpecification{
			LaunchTemplateName: aws.String(templateName),
			Version:            aws.String("2"),
		},
	})
	require.NoError(t, err, "run-instances with explicit launch-template version 2")
	gotV2 := awaitLaunchTemplateNodeInput(t, nodeCh)
	require.Equal(t, amiV2, aws.StringValue(gotV2.ImageId), "explicit Version=2 must resolve version 2's data")
}

// TestRunInstances_LaunchTemplateFieldPrecedence proves mergeRunInstancesInput
// overlays direct RunInstances parameters onto the template-derived base:
// a field left nil on the direct request inherits the template, but a field
// explicitly set on the direct request wins over the template's value.
func TestRunInstances_LaunchTemplateFieldPrecedence(t *testing.T) {
	gw := StartGateway(t)
	StartLaunchTemplateDaemonLite(t, gw)

	const (
		templateName = "lt-field-precedence"
		instanceType = "t3.micro"
		nodeID       = "lt-node"
		templateAMI  = "ami-0000000000000fff"
	)

	gw.StubSubject(t, "spinifex.node.status", mustMarshal(t, &types.NodeStatusResponse{
		Node:          nodeID,
		InstanceTypes: []types.InstanceTypeCap{{Name: instanceType, Available: 5}},
	}))
	nodeCh := captureLaunchTemplateNodeInput(t, gw, instanceType, nodeID)

	_, err := gw.EC2Client(t).CreateLaunchTemplate(&ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(templateName),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			ImageId:      aws.String(templateAMI),
			InstanceType: aws.String(instanceType),
			KeyName:      aws.String("template-key"),
		},
	})
	require.NoError(t, err, "create-launch-template")

	_, err = gw.EC2Client(t).RunInstances(&ec2.RunInstancesInput{
		MinCount: aws.Int64(1),
		MaxCount: aws.Int64(1),
		KeyName:  aws.String("explicit-key"),
		LaunchTemplate: &ec2.LaunchTemplateSpecification{
			LaunchTemplateName: aws.String(templateName),
		},
	})
	require.NoError(t, err, "run-instances overriding KeyName")

	got := awaitLaunchTemplateNodeInput(t, nodeCh)
	require.Equal(t, "explicit-key", aws.StringValue(got.KeyName), "an explicit direct-request field must override the template's value")
	require.Equal(t, templateAMI, aws.StringValue(got.ImageId), "a field left unset on the direct request must inherit the template")
}
