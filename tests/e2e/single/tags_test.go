//go:build e2e

package single

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// phase6_TagManagement exercises CreateTags / DescribeTags / DeleteTags
// against the Phase 5 instance and its root volume. Maps to run-e2e.sh
// ~930–1026. Sub-steps mirror the bash 6a–6j checks; we additionally
// assert in 6i that the value-conditional DeleteTags scoped to the
// instance does NOT touch the matching tag on the root volume, which the
// bash driver only implies via per-resource targeting.
func phase6_TagManagement(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 6 — Tag Management")
	require.NotEmpty(t, fix.InstanceID, "Phase 5 must populate fix.InstanceID")
	require.NotEmpty(t, fix.RootVolumeID, "Phase 5 must populate fix.RootVolumeID")

	// Cleanup runs regardless of which sub-step failed so later phases see
	// a tag-free instance/volume. DeleteTags with no Value drops the tag
	// unconditionally — exactly what we want here.
	t.Cleanup(func() {
		_, _ = fix.AWS.EC2.DeleteTags(&ec2.DeleteTagsInput{
			Resources: []*string{aws.String(fix.InstanceID), aws.String(fix.RootVolumeID)},
			Tags: []*ec2.Tag{
				{Key: aws.String("Name")},
				{Key: aws.String("Environment")},
				{Key: aws.String("DeleteMe")},
			},
		})
	})

	// 6a: create three tags on the instance.
	harness.Step(t, "6a create-tags instance=%s (Name, Environment, DeleteMe)", fix.InstanceID)
	_, err := fix.AWS.EC2.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(fix.InstanceID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("e2e-test")},
			{Key: aws.String("Environment"), Value: aws.String("testing")},
			{Key: aws.String("DeleteMe"), Value: aws.String("please")},
		},
	})
	require.NoError(t, err, "create-tags instance")

	// 6b: filter by resource-id should return all three.
	harness.Step(t, "6b describe-tags resource-id=%s want=3", fix.InstanceID)
	tags, err := describeTags(fix, &ec2.Filter{
		Name:   aws.String("resource-id"),
		Values: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "describe-tags resource-id")
	require.Lenf(t, tags, 3, "instance tag count after 6a: %v", tagSummary(tags))

	// 6c: tag the root volume with two tags. Bash uses VOLUME_ID (the now-
	// deleted Phase 5b volume), which is a bug in the bash driver; the Go
	// port targets the persistent RootVolumeID so the assertion in 6d
	// reflects real cross-resource state.
	harness.Step(t, "6c create-tags volume=%s (Name, Environment)", fix.RootVolumeID)
	_, err = fix.AWS.EC2.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(fix.RootVolumeID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("e2e-root-vol")},
			{Key: aws.String("Environment"), Value: aws.String("testing")},
		},
	})
	require.NoError(t, err, "create-tags volume")

	// 6d: filter by key=Environment should return exactly 2 (one per resource).
	harness.Step(t, "6d describe-tags key=Environment want=2")
	tags, err = describeTags(fix, &ec2.Filter{
		Name:   aws.String("key"),
		Values: []*string{aws.String("Environment")},
	})
	require.NoError(t, err, "describe-tags key=Environment")
	require.Lenf(t, tags, 2, "Environment tag count across resources: %v", tagSummary(tags))

	// 6e: filter by resource-type=instance should return 3 (all instance tags).
	harness.Step(t, "6e describe-tags resource-type=instance want=3")
	tags, err = describeTags(fix, &ec2.Filter{
		Name:   aws.String("resource-type"),
		Values: []*string{aws.String("instance")},
	})
	require.NoError(t, err, "describe-tags resource-type=instance")
	require.Lenf(t, tags, 3, "instance-typed tag count: %v", tagSummary(tags))

	// 6f: overwrite Name on the instance — CreateTags with the same Key
	// must replace the prior Value.
	harness.Step(t, "6f overwrite instance Name -> e2e-test-updated")
	_, err = fix.AWS.EC2.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(fix.InstanceID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("e2e-test-updated")},
		},
	})
	require.NoError(t, err, "create-tags overwrite Name")

	tags, err = describeTags(fix,
		&ec2.Filter{Name: aws.String("resource-id"), Values: []*string{aws.String(fix.InstanceID)}},
		&ec2.Filter{Name: aws.String("key"), Values: []*string{aws.String("Name")}},
	)
	require.NoError(t, err, "describe-tags resource-id + key=Name")
	require.Lenf(t, tags, 1, "expected exactly one Name tag on instance: %v", tagSummary(tags))
	assert.Equal(t, "e2e-test-updated", aws.StringValue(tags[0].Value), "Name tag value after overwrite")

	// 6g: delete-tags with no Value drops the tag regardless of stored
	// value. Instance should be down to {Name, Environment}.
	harness.Step(t, "6g delete-tags Key=DeleteMe (no value)")
	_, err = fix.AWS.EC2.DeleteTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(fix.InstanceID)},
		Tags:      []*ec2.Tag{{Key: aws.String("DeleteMe")}},
	})
	require.NoError(t, err, "delete-tags DeleteMe")

	tags, err = describeTags(fix, &ec2.Filter{
		Name:   aws.String("resource-id"),
		Values: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "describe-tags after unconditional delete")
	require.Lenf(t, tags, 2, "instance tags after deleting DeleteMe: %v", tagSummary(tags))

	// 6h: delete-tags with a non-matching Value must be a no-op — the
	// Environment tag on the instance still holds "testing".
	harness.Step(t, "6h delete-tags Key=Environment,Value=production (no-op)")
	_, err = fix.AWS.EC2.DeleteTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(fix.InstanceID)},
		Tags:      []*ec2.Tag{{Key: aws.String("Environment"), Value: aws.String("production")}},
	})
	require.NoError(t, err, "delete-tags Environment=production")

	tags, err = describeTags(fix,
		&ec2.Filter{Name: aws.String("resource-id"), Values: []*string{aws.String(fix.InstanceID)}},
		&ec2.Filter{Name: aws.String("key"), Values: []*string{aws.String("Environment")}},
	)
	require.NoError(t, err, "describe-tags instance Environment after no-op delete")
	require.Lenf(t, tags, 1, "Environment tag must survive non-matching delete: %v", tagSummary(tags))
	assert.Equal(t, "testing", aws.StringValue(tags[0].Value), "Environment value after no-op delete")

	// 6i: delete-tags with the correct Value removes the tag from the
	// targeted resource only. The volume's Environment tag must survive
	// because DeleteTags was scoped to the instance.
	harness.Step(t, "6i delete-tags Key=Environment,Value=testing (instance only)")
	_, err = fix.AWS.EC2.DeleteTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(fix.InstanceID)},
		Tags:      []*ec2.Tag{{Key: aws.String("Environment"), Value: aws.String("testing")}},
	})
	require.NoError(t, err, "delete-tags Environment=testing")

	tags, err = describeTags(fix,
		&ec2.Filter{Name: aws.String("resource-id"), Values: []*string{aws.String(fix.InstanceID)}},
		&ec2.Filter{Name: aws.String("key"), Values: []*string{aws.String("Environment")}},
	)
	require.NoError(t, err, "describe-tags instance Environment after match-delete")
	assert.Lenf(t, tags, 0, "Environment tag must be gone from instance: %v", tagSummary(tags))

	tags, err = describeTags(fix,
		&ec2.Filter{Name: aws.String("resource-id"), Values: []*string{aws.String(fix.RootVolumeID)}},
		&ec2.Filter{Name: aws.String("key"), Values: []*string{aws.String("Environment")}},
	)
	require.NoError(t, err, "describe-tags volume Environment after instance-scoped delete")
	require.Lenf(t, tags, 1, "volume Environment tag must survive instance-scoped delete: %v", tagSummary(tags))
	assert.Equal(t, fix.RootVolumeID, aws.StringValue(tags[0].ResourceId), "Environment tag still attached to root volume")

	// 6j: only Name should remain on the instance.
	harness.Step(t, "6j final describe-tags resource-id=%s want=1", fix.InstanceID)
	tags, err = describeTags(fix, &ec2.Filter{
		Name:   aws.String("resource-id"),
		Values: []*string{aws.String(fix.InstanceID)},
	})
	require.NoError(t, err, "describe-tags final instance")
	require.Lenf(t, tags, 1, "final instance tag count: %v", tagSummary(tags))
	assert.Equal(t, "Name", aws.StringValue(tags[0].Key), "final remaining instance tag key")
}

// describeTags is a thin wrapper that builds a DescribeTagsInput from the
// supplied filters — the test reaches for it once per sub-step so the
// assertion lines stay readable.
func describeTags(fix *Fixture, filters ...*ec2.Filter) ([]*ec2.TagDescription, error) {
	out, err := fix.AWS.EC2.DescribeTags(&ec2.DescribeTagsInput{Filters: filters})
	if err != nil {
		return nil, err
	}
	return out.Tags, nil
}

// tagSummary formats a TagDescription slice as resource=key=value triples
// for assertion failure messages — bare counts hide which tag is wrong.
func tagSummary(tags []*ec2.TagDescription) []string {
	out := make([]string, 0, len(tags))
	for _, td := range tags {
		out = append(out, aws.StringValue(td.ResourceId)+"="+aws.StringValue(td.Key)+"="+aws.StringValue(td.Value))
	}
	return out
}
