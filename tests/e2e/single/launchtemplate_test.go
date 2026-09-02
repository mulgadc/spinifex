//go:build e2e

package single

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// ltUDMarker / ltUDDoneFile prove the template's UserData survived the
	// whole-field merge, the base64 round trip and IMDS, and was actually run
	// by cloud-init in the guest — not merely stored on the instance record.
	ltUDMarker   = "e2e-lt-userdata-marker-4b19d7"
	ltUDDoneFile = "/run/e2e-lt-userdata.done"

	// ltRootDevice is the guest-visible root device name. parseVolumeParams
	// reads BlockDeviceMappings[0].DeviceName straight off the RunInstances
	// request and defaults to exactly this string.
	ltRootDevice = "/dev/vda"

	// ltRootGrowthGiB is how far above the AMI's own root size the template
	// sizes the root volume. floorVolumeSizeToAMI rounds a smaller request up
	// to the AMI snapshot size, so a dropped BlockDeviceMappings and an
	// honoured one would be indistinguishable at or below that floor.
	ltRootGrowthGiB = 4

	// ltUDMarkerBudget bounds the wait for cloud-init's runcmd. SSH becomes
	// reachable before cloud-final completes, so the marker file lags the
	// handshake by a variable amount.
	ltUDMarkerBudget = 90 * time.Second
)

// ltUserData is a #cloud-config whose runcmd writes ltUDMarker to
// ltUDDoneFile. Carried on the template base64-encoded, as AWS requires of
// LaunchTemplateData.UserData.
const ltUserData = "#cloud-config\n" +
	"# " + ltUDMarker + "\n" +
	"runcmd:\n" +
	"  - [ sh, -c, \"echo " + ltUDMarker + " > " + ltUDDoneFile + "\" ]\n"

// runLaunchTemplateBoot launches one instance from a launch template alone and
// asserts the template's fields landed on the real VM.
//
// The control-plane half of launch templates — CRUD, tag-filtered describe,
// SourceVersion merge, $Latest, version deletion, and the template-vs-inline
// field precedence — is owned by the integration tier
// (tests/integration/launchtemplate_test.go and
// run_instances_launchtemplate_test.go) and is deliberately not repeated here.
// Those tests stop at the per-node ec2.RunInstances.<type>.<node> subject,
// where a stub responder replies with a canned Reservation, so they observe
// only the three fields they capture off that request.
//
// ExpandRunInstances merges every field of the template into RunInstancesInput
// (mappers.go's mergeWholeField), so a merge that drops UserData, or emits a
// BlockDeviceMappings shape the daemon's launch path ignores, produces a green
// integration run and a broken product. That is the gap this test closes: an
// LT-expanded launch reaching a real daemon and producing a booting VM whose
// guest-visible state matches the template.
func runLaunchTemplateBoot(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — launch-template-sourced fields on a booted guest")

	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)
	amiID := needAMI(t, fix)
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, vpc.SGID, "default SG ID required")
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	rootGiB := ltRootVolumeGiB(t, fix, amiID)
	tagValue := fmt.Sprintf("e2e-lt-%d", time.Now().UnixNano())
	harness.Detail(t, "root_gib", rootGiB, "tag_value", tagValue)

	templateName := "e2e-lt-boot-" + tagValue
	// e2e:allow-create — the template under test; no harness fixture creates one.
	createOut, err := fix.AWS.EC2.CreateLaunchTemplate(&ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String(templateName),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			ImageId:      aws.String(amiID),
			InstanceType: aws.String(instType),
			KeyName:      aws.String(keyName),
			UserData:     aws.String(base64.StdEncoding.EncodeToString([]byte(ltUserData))),
			NetworkInterfaces: []*ec2.LaunchTemplateInstanceNetworkInterfaceSpecificationRequest{{
				DeviceIndex:              aws.Int64(0),
				SubnetId:                 aws.String(vpc.SubnetID),
				Groups:                   []*string{aws.String(vpc.SGID)},
				AssociatePublicIpAddress: aws.Bool(true),
			}},
			BlockDeviceMappings: []*ec2.LaunchTemplateBlockDeviceMappingRequest{{
				DeviceName: aws.String(ltRootDevice),
				Ebs: &ec2.LaunchTemplateEbsBlockDeviceRequest{
					VolumeSize:          aws.Int64(int64(rootGiB)),
					DeleteOnTermination: aws.Bool(true),
				},
			}},
			TagSpecifications: []*ec2.LaunchTemplateTagSpecificationRequest{{
				ResourceType: aws.String(ec2.ResourceTypeInstance),
				Tags:         []*ec2.Tag{{Key: aws.String("e2e-lt"), Value: aws.String(tagValue)}},
			}},
		},
	})
	require.NoError(t, err, "create-launch-template %s", templateName)
	require.NotNil(t, createOut.LaunchTemplate, "create-launch-template returned no template")
	templateID := aws.StringValue(createOut.LaunchTemplate.LaunchTemplateId)
	require.NotEmpty(t, templateID, "create-launch-template returned an empty id")

	// Registered before any instance cleanup so it runs last (LIFO): the
	// template outlives the VMs launched from it.
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.DeleteLaunchTemplate(&ec2.DeleteLaunchTemplateInput{
			LaunchTemplateId: aws.String(templateID),
		})
	})

	// No ImageId, InstanceType, SubnetId, SecurityGroupIds or KeyName here. If
	// expansion did not run, ValidateRunInstancesInput rejects the request for
	// a missing ImageId before it ever reaches a node, so the launch itself is
	// the first assertion.
	harness.Step(t, "run-instances --launch-template %s (no inline parameters)", templateID)
	instanceID := launchFromTemplate(t, fix, &ec2.LaunchTemplateSpecification{
		LaunchTemplateId: aws.String(templateID),
	})
	inst := describeSingletonInstance(t, fix, instanceID)

	host, port := harness.InstancePublicSSHHost(t, inst)
	harness.Detail(t, "instance_id", instanceID, "ssh_host", host, "ssh_port", port)
	waitForSSHReady(t, host, port, keyPath)
	tgt := harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}

	harness.Step(t, "guest: template user-data ran")
	waitForLTUserDataMarker(t, tgt)

	harness.Step(t, "guest: root disk matches the template's BlockDeviceMappings size")
	guestGiB := harness.LsblkRootGiB(t, tgt)
	// lsblk rounds down, and the guest's view can sit a hair under the API
	// size, so ±1 GiB — the same tolerance the singleton boot check uses.
	assert.LessOrEqualf(t, absInt(guestGiB-rootGiB), 1,
		"guest root disk %d GiB does not match the template's %d GiB: BlockDeviceMappings was dropped or overridden",
		guestGiB, rootGiB)
	harness.Detail(t, "guest_gib", guestGiB, "template_gib", rootGiB)

	harness.Step(t, "describe-instances: API-observable template fields")
	assert.Equal(t, instType, aws.StringValue(inst.InstanceType), "instance type must come from the template")
	assert.Equal(t, keyName, aws.StringValue(inst.KeyName), "key name must come from the template")
	assert.Equal(t, vpc.SubnetID, aws.StringValue(inst.SubnetId), "subnet must come from the template's NetworkInterfaces")
	assert.Contains(t, instanceSGIDs(inst), vpc.SGID, "security group must come from the template's NetworkInterfaces")
	assert.Equal(t, tagValue, instanceTagValue(inst, "e2e-lt"), "instance tag must come from the template's TagSpecifications")

	// Version pinning is the one control-plane behaviour worth repeating on
	// the live tier: the integration tier proves the gateway selected the
	// pinned version's value, not that the daemon honoured it end to end.
	pinnedType := strings.TrimSuffix(instType, ".nano") + ".small"
	require.NotEqualf(t, instType, pinnedType, "could not derive a second instance type from %q", instType)

	harness.Step(t, "create-launch-template-version 2 type=%s and make it $Default", pinnedType)
	// e2e:allow-create — v2 exists only to move $Default off the pinned version.
	v2Out, err := fix.AWS.EC2.CreateLaunchTemplateVersion(&ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId: aws.String(templateID),
		SourceVersion:    aws.String("1"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{
			InstanceType: aws.String(pinnedType),
		},
	})
	require.NoError(t, err, "create-launch-template-version")
	require.NotNil(t, v2Out.LaunchTemplateVersion, "create-launch-template-version returned no version")
	require.Equal(t, int64(2), aws.Int64Value(v2Out.LaunchTemplateVersion.VersionNumber))

	_, err = fix.AWS.EC2.ModifyLaunchTemplate(&ec2.ModifyLaunchTemplateInput{
		LaunchTemplateId: aws.String(templateID),
		DefaultVersion:   aws.String("2"),
	})
	require.NoError(t, err, "modify-launch-template --default-version 2")

	harness.Step(t, "run-instances pinned to version 1 while $Default is version 2")
	pinnedID := launchFromTemplate(t, fix, &ec2.LaunchTemplateSpecification{
		LaunchTemplateId: aws.String(templateID),
		Version:          aws.String("1"),
	})
	pinned := describeSingletonInstance(t, fix, pinnedID)
	assert.Equalf(t, instType, aws.StringValue(pinned.InstanceType),
		"pinned launch must use version 1's instance type, not $Default version 2's %s", pinnedType)

	// This instance only had to reach running; it never needs to finish
	// booting, so reclaim its node capacity now rather than at cleanup.
	harness.Step(t, "terminate-instances %s", pinnedID)
	_, err = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(pinnedID)},
	})
	require.NoError(t, err, "terminate-instances %s", pinnedID)
	harness.WaitForInstanceState(t, fix.AWS, pinnedID, "terminated")
}

// launchFromTemplate runs one instance from spec alone, registers a
// terminate-and-wait cleanup, and returns its id once it reaches running.
// Deliberately passes nothing but the template reference and the counts —
// any inline parameter here would mask a merge that failed to supply it.
func launchFromTemplate(t *testing.T, fix *Fixture, spec *ec2.LaunchTemplateSpecification) string {
	t.Helper()
	// e2e:allow-create — launching from the template is the subject under test.
	out, err := fix.AWS.EC2.RunInstances(&ec2.RunInstancesInput{
		LaunchTemplate: spec,
		MinCount:       aws.Int64(1),
		MaxCount:       aws.Int64(1),
	})
	require.NoError(t, err, "run-instances --launch-template")
	require.NotEmpty(t, out.Instances, "run-instances returned no instances")
	id := aws.StringValue(out.Instances[0].InstanceId)
	require.NotEmpty(t, id, "run-instances returned an empty instance id")
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(id)},
		})
		harness.WaitForInstanceState(t, fix.AWS, id, "terminated")
	})
	harness.WaitForInstanceState(t, fix.AWS, id, "running")
	return id
}

// ltRootVolumeGiB returns a root size strictly larger than amiID's own, so an
// honoured BlockDeviceMappings is distinguishable from a dropped one. The AMI's
// size comes from the synthesized root mapping DescribeImages reports.
func ltRootVolumeGiB(t *testing.T, fix *Fixture, amiID string) int {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{aws.String(amiID)},
	})
	require.NoError(t, err, "describe-images %s", amiID)
	require.NotEmpty(t, out.Images, "no image for %s", amiID)

	img := out.Images[0]
	rootDev := aws.StringValue(img.RootDeviceName)
	for _, bdm := range img.BlockDeviceMappings {
		if rootDev != "" && aws.StringValue(bdm.DeviceName) != rootDev {
			continue
		}
		if bdm.Ebs == nil {
			continue
		}
		if size := int(aws.Int64Value(bdm.Ebs.VolumeSize)); size > 0 {
			return size + ltRootGrowthGiB
		}
	}
	t.Fatalf("AMI %s reports no root volume size; cannot size the template's root above it", amiID)
	return 0
}

// waitForLTUserDataMarker polls the guest until cloud-init's runcmd has
// written ltUDMarker. SSH is reachable well before cloud-final finishes, so
// the marker lags the handshake.
func waitForLTUserDataMarker(t *testing.T, tgt harness.SSHTarget) {
	t.Helper()
	deadline := time.Now().Add(ltUDMarkerBudget)
	var last string
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		out, err := harness.RunGuestSSH(ctx, tgt, "cat "+ltUDDoneFile+" 2>/dev/null")
		cancel()
		if err == nil {
			last = strings.TrimSpace(string(out))
			if strings.Contains(last, ltUDMarker) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("template user-data marker %s never appeared in %s within %s (last read %q) — "+
				"the template's UserData did not survive the launch-template merge",
				ltUDMarker, ltUDDoneFile, ltUDMarkerBudget, last)
		}
		time.Sleep(3 * time.Second)
	}
}

// instanceSGIDs returns the security group ids attached to inst.
func instanceSGIDs(inst *ec2.Instance) []string {
	ids := make([]string, 0, len(inst.SecurityGroups))
	for _, sg := range inst.SecurityGroups {
		ids = append(ids, aws.StringValue(sg.GroupId))
	}
	return ids
}

// instanceTagValue returns inst's value for key, or "" when absent.
func instanceTagValue(inst *ec2.Instance, key string) string {
	for _, tag := range inst.Tags {
		if aws.StringValue(tag.Key) == key {
			return aws.StringValue(tag.Value)
		}
	}
	return ""
}

// absInt returns the absolute value of n.
func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
