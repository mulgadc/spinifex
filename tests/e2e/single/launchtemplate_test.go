//go:build e2e

package single

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runLaunchTemplates drives the launch-template control plane through the
// deployed AWS gateway, then launches an instance from the selected default
// version. It intentionally uses parentheses in the name: they are valid AWS
// launch-template characters but require the service's KV name-index encoding.
func runLaunchTemplates(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — EC2 Launch Templates")

	amiID := needAMI(t, fix)
	instanceType, _ := needInstanceTypeArch(t, fix)
	keyName, _ := needKeyPair(t, fix)
	templateName := fmt.Sprintf("e2e-lt-(%s)", fix.Harness.Scratch())

	// e2e:allow-create — the launch template lifecycle is the subject under test.
	createOut, err := fix.AWS.EC2.CreateLaunchTemplate(&ec2.CreateLaunchTemplateInput{
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
				{Key: aws.String("e2e-suite"), Value: aws.String(fix.Harness.Scratch())},
			},
		}},
	})
	require.NoError(t, err, "create-launch-template")
	require.NotNil(t, createOut.LaunchTemplate, "create-launch-template returned no template")
	templateID := aws.StringValue(createOut.LaunchTemplate.LaunchTemplateId)
	require.NotEmpty(t, templateID, "create-launch-template returned an empty id")
	assert.Equal(t, int64(1), aws.Int64Value(createOut.LaunchTemplate.DefaultVersionNumber))

	templateDeleted := false
	t.Cleanup(func() {
		if templateDeleted {
			return
		}
		if _, err := fix.AWS.EC2.DeleteLaunchTemplate(&ec2.DeleteLaunchTemplateInput{
			LaunchTemplateName: aws.String(templateName),
		}); err != nil {
			t.Logf("cleanup delete-launch-template %q: %v", templateName, err)
		}
	})

	harness.Step(t, "describe-launch-templates by launch-template tag")
	descTemplates, err := fix.AWS.EC2.DescribeLaunchTemplates(&ec2.DescribeLaunchTemplatesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("tag:e2e-suite"),
			Values: []*string{aws.String(fix.Harness.Scratch())},
		}},
	})
	require.NoError(t, err, "describe-launch-templates by tag")
	require.Len(t, descTemplates.LaunchTemplates, 1)
	assert.Equal(t, templateID, aws.StringValue(descTemplates.LaunchTemplates[0].LaunchTemplateId))

	// SourceVersion inherits the AMI and instance type from v1 while the
	// non-nil UserData field replaces only that whole field.
	const userData = "ZWNobyBsYXVuY2gtdGVtcGxhdGUK"
	// e2e:allow-create — version creation is part of the launch-template lifecycle under test.
	v2Out, err := fix.AWS.EC2.CreateLaunchTemplateVersion(&ec2.CreateLaunchTemplateVersionInput{
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

	harness.Step(t, "describe-launch-template-versions $Latest by name")
	latestOut, err := fix.AWS.EC2.DescribeLaunchTemplateVersions(&ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateName: aws.String(templateName),
		Versions:           []*string{aws.String("$Latest")},
	})
	require.NoError(t, err, "describe-launch-template-versions $Latest")
	require.Len(t, latestOut.LaunchTemplateVersions, 1)
	assert.Equal(t, int64(2), aws.Int64Value(latestOut.LaunchTemplateVersions[0].VersionNumber))

	harness.Step(t, "modify-launch-template default version -> 2")
	modifyOut, err := fix.AWS.EC2.ModifyLaunchTemplate(&ec2.ModifyLaunchTemplateInput{
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
	// e2e:allow-create — version creation is part of the launch-template lifecycle under test.
	v3Out, err := fix.AWS.EC2.CreateLaunchTemplateVersion(&ec2.CreateLaunchTemplateVersionInput{
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

	latestOut, err = fix.AWS.EC2.DescribeLaunchTemplateVersions(&ec2.DescribeLaunchTemplateVersionsInput{
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

	deleteVersionsOut, err := fix.AWS.EC2.DeleteLaunchTemplateVersions(&ec2.DeleteLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(templateID),
		Versions:         []*string{aws.String("3")},
	})
	require.NoError(t, err, "delete-launch-template-versions")
	require.Len(t, deleteVersionsOut.SuccessfullyDeletedLaunchTemplateVersions, 1)
	assert.Equal(t, int64(3), aws.Int64Value(deleteVersionsOut.SuccessfullyDeletedLaunchTemplateVersions[0].VersionNumber))
	assert.Empty(t, deleteVersionsOut.UnsuccessfullyDeletedLaunchTemplateVersions)

	latestOut, err = fix.AWS.EC2.DescribeLaunchTemplateVersions(&ec2.DescribeLaunchTemplateVersionsInput{
		LaunchTemplateId: aws.String(templateID),
		Versions:         []*string{aws.String("$Latest")},
	})
	require.NoError(t, err, "describe-launch-template-versions $Latest after delete")
	require.Len(t, latestOut.LaunchTemplateVersions, 1)
	assert.Equal(t, int64(2), aws.Int64Value(latestOut.LaunchTemplateVersions[0].VersionNumber))

	// RunInstances from the default launch-template version (resolving the
	// template's ImageId/InstanceType onto the per-node launch dispatch, then
	// waiting for a real boot to read them back off DescribeInstances) is
	// covered by tests/integration's
	// TestRunInstances_LaunchTemplateExpansion — that expansion is gateway-side
	// logic, fully resolved before the daemon-facing NATS hop, so a live guest
	// proves nothing beyond what the integration tier already asserts.

	harness.Step(t, "delete-launch-template by name")
	deleteOut, err := fix.AWS.EC2.DeleteLaunchTemplate(&ec2.DeleteLaunchTemplateInput{
		LaunchTemplateName: aws.String(templateName),
	})
	require.NoError(t, err, "delete-launch-template")
	require.NotNil(t, deleteOut.LaunchTemplate)
	assert.Equal(t, templateID, aws.StringValue(deleteOut.LaunchTemplate.LaunchTemplateId))
	templateDeleted = true
}
