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
	"github.com/stretchr/testify/assert"
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
// touched a guest either; it lives alongside this test in
// TestLaunchTemplates_CRUDLifecycle below.
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

// TestLaunchTemplates_CRUDLifecycle ports the live e2e launch-template
// control-plane lifecycle (formerly tests/e2e/single/launchtemplate_test.go)
// off the live tier: create, tag-filtered describe, a SourceVersion-merged
// version, $Latest resolution, a default-version change, a nested timestamp
// (InstanceMarketOptions.SpotOptions.ValidUntil) round-tripping through the
// EC2 query encode/decode, version deletion, and template deletion. None of
// it ever launches a guest: every operation resolves entirely against
// LaunchTemplateServiceImpl's KV store via StartLaunchTemplateDaemonLite, the
// same production handler code (handlers/ec2/launchtemplate/service_impl.go)
// a live daemon runs. RunInstances template expansion — the one launch-
// template-adjacent behaviour that does run gateway-side before a per-node
// dispatch — is covered separately by TestRunInstances_LaunchTemplateExpansion
// above.
func TestLaunchTemplates_CRUDLifecycle(t *testing.T) {
	gw := StartGateway(t)
	StartLaunchTemplateDaemonLite(t, gw)

	client := gw.EC2Client(t)

	const (
		amiID        = "ami-0123456789abcdef0"
		instanceType = "t3.micro"
		keyName      = "integration-lt-keypair"
	)
	// Parentheses are valid AWS launch-template name characters but require the
	// service's KV name-index hex encoding (nameKey in service_impl.go).
	const templateName = "integration-lt-(crud)"
	const tagValue = "integration-lt-suite"

	createOut, err := client.CreateLaunchTemplate(&ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(templateName),
		VersionDescription: aws.String("initial version"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			ImageId:      aws.String(amiID),
			InstanceType: aws.String(instanceType),
			KeyName:      aws.String(keyName),
		},
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("launch-template"),
			Tags: []*ec2.Tag{
				{Key: aws.String("e2e-suite"), Value: aws.String(tagValue)},
			},
		}},
	})
	require.NoError(t, err, "create-launch-template")
	require.NotNil(t, createOut.LaunchTemplate, "create-launch-template returned no template")
	templateID := aws.StringValue(createOut.LaunchTemplate.LaunchTemplateId)
	require.NotEmpty(t, templateID, "create-launch-template returned an empty id")
	assert.Equal(t, int64(1), aws.Int64Value(createOut.LaunchTemplate.DefaultVersionNumber))

	t.Cleanup(func() {
		// Best-effort: the lifecycle below deletes the template itself on the
		// success path, so a second delete here is expected to no-op/error.
		_, _ = client.DeleteLaunchTemplate(&ec2.DeleteLaunchTemplateInput{LaunchTemplateName: aws.String(templateName)})
	})

	descTemplates, err := client.DescribeLaunchTemplates(&ec2.DescribeLaunchTemplatesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("tag:e2e-suite"),
			Values: []*string{aws.String(tagValue)},
		}},
	})
	require.NoError(t, err, "describe-launch-templates by tag")
	require.Len(t, descTemplates.LaunchTemplates, 1)
	assert.Equal(t, templateID, aws.StringValue(descTemplates.LaunchTemplates[0].LaunchTemplateId))

	// SourceVersion inherits the AMI and instance type from v1 while the
	// non-nil UserData field replaces only that whole field.
	const userData = "ZWNobyBsYXVuY2gtdGVtcGxhdGUK"
	v2Out, err := client.CreateLaunchTemplateVersion(&ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateName: aws.String(templateName),
		SourceVersion:      aws.String("1"),
		VersionDescription: aws.String("source merge version"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			UserData: aws.String(userData),
		},
	})
	require.NoError(t, err, "create-launch-template-version from source")
	require.NotNil(t, v2Out.LaunchTemplateVersion, "create-launch-template-version returned no version")
	v2 := v2Out.LaunchTemplateVersion
	assert.Equal(t, int64(2), aws.Int64Value(v2.VersionNumber))
	require.NotNil(t, v2.LaunchTemplateData)
	assert.Equal(t, amiID, aws.StringValue(v2.LaunchTemplateData.ImageId))
	assert.Equal(t, instanceType, aws.StringValue(v2.LaunchTemplateData.InstanceType))
	assert.Equal(t, userData, aws.StringValue(v2.LaunchTemplateData.UserData))

	latestOut, err := client.DescribeLaunchTemplateVersions(&ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateName: aws.String(templateName),
		Versions:           []*string{aws.String("$Latest")},
	})
	require.NoError(t, err, "describe-launch-template-versions $Latest")
	require.Len(t, latestOut.LaunchTemplateVersions, 1)
	assert.Equal(t, int64(2), aws.Int64Value(latestOut.LaunchTemplateVersions[0].VersionNumber))

	modifyOut, err := client.ModifyLaunchTemplate(&ec2.ModifyLaunchTemplateInput{
		LaunchTemplateId: aws.String(templateID),
		DefaultVersion:   aws.String("2"),
	})
	require.NoError(t, err, "modify-launch-template")
	require.NotNil(t, modifyOut.LaunchTemplate)
	assert.Equal(t, int64(2), aws.Int64Value(modifyOut.LaunchTemplate.DefaultVersionNumber))

	// Create and remove a tail version so DeleteLaunchTemplateVersions and
	// $Latest-after-delete are covered without disturbing the default v2. This
	// version also drives a nested timestamp through the EC2 query parser.
	validUntil := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	v3Out, err := client.CreateLaunchTemplateVersion(&ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId: aws.String(templateID),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			InstanceType: aws.String(instanceType),
			InstanceMarketOptions: &ec2.LaunchTemplateInstanceMarketOptionsRequest{
				MarketType: aws.String("spot"),
				SpotOptions: &ec2.LaunchTemplateSpotMarketOptionsRequest{
					ValidUntil: aws.Time(validUntil),
				},
			},
		},
	})
	require.NoError(t, err, "create-launch-template-version tail")
	require.NotNil(t, v3Out.LaunchTemplateVersion)
	assert.Equal(t, int64(3), aws.Int64Value(v3Out.LaunchTemplateVersion.VersionNumber))

	latestOut, err = client.DescribeLaunchTemplateVersions(&ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(templateID),
		Versions:         []*string{aws.String("$Latest")},
	})
	require.NoError(t, err, "describe-launch-template-versions $Latest tail")
	require.Len(t, latestOut.LaunchTemplateVersions, 1)
	require.NotNil(t, latestOut.LaunchTemplateVersions[0].LaunchTemplateData)
	require.NotNil(t, latestOut.LaunchTemplateVersions[0].LaunchTemplateData.InstanceMarketOptions)
	require.NotNil(t, latestOut.LaunchTemplateVersions[0].LaunchTemplateData.InstanceMarketOptions.SpotOptions)
	require.NotNil(t, latestOut.LaunchTemplateVersions[0].LaunchTemplateData.InstanceMarketOptions.SpotOptions.ValidUntil)
	assert.Equal(t, validUntil, *latestOut.LaunchTemplateVersions[0].LaunchTemplateData.InstanceMarketOptions.SpotOptions.ValidUntil)

	deleteVersionsOut, err := client.DeleteLaunchTemplateVersions(&ec2.DeleteLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(templateID),
		Versions:         []*string{aws.String("3")},
	})
	require.NoError(t, err, "delete-launch-template-versions")
	require.Len(t, deleteVersionsOut.SuccessfullyDeletedLaunchTemplateVersions, 1)
	assert.Equal(t, int64(3), aws.Int64Value(deleteVersionsOut.SuccessfullyDeletedLaunchTemplateVersions[0].VersionNumber))
	assert.Empty(t, deleteVersionsOut.UnsuccessfullyDeletedLaunchTemplateVersions)

	latestOut, err = client.DescribeLaunchTemplateVersions(&ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(templateID),
		Versions:         []*string{aws.String("$Latest")},
	})
	require.NoError(t, err, "describe-launch-template-versions $Latest after delete")
	require.Len(t, latestOut.LaunchTemplateVersions, 1)
	assert.Equal(t, int64(2), aws.Int64Value(latestOut.LaunchTemplateVersions[0].VersionNumber))

	deleteOut, err := client.DeleteLaunchTemplate(&ec2.DeleteLaunchTemplateInput{
		LaunchTemplateName: aws.String(templateName),
	})
	require.NoError(t, err, "delete-launch-template")
	require.NotNil(t, deleteOut.LaunchTemplate)
	assert.Equal(t, templateID, aws.StringValue(deleteOut.LaunchTemplate.LaunchTemplateId))
}
