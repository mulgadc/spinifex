//go:build integration

package integration

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTagManagement is ported from tests/e2e/single/tags_test.go
// runTagManagement, driving the REAL tags/service_impl.go over the harness's
// DaemonLite. The E2E source tags a running instance and its root EBS
// volume; instance lifecycle and volume creation are out of scope for this
// tier (they need vm.Manager/QEMU and viperblock respectively — see
// mulga-xaa9f), so this port substitutes two in-scope resources that stand
// in for the same two-resource-isolation shape: a scratch VPC (stand-in for
// "instance") and its own auto-created main route table (stand-in for "root
// volume"). CreateTags/DescribeTags/DeleteTags never inspect resource
// existence or type beyond the ID string (handlers/ec2/tags/service_impl.go
// getResourceType), so every assertion below — per-resource tag counts,
// resource-id/resource-type/key filtering, Name overwrite-on-recreate,
// unconditional vs. value-scoped delete-tags, cross-resource isolation —
// exercises the identical code path the E2E source exercised, just against
// "vpc" and "route-table" resource-type strings instead of "instance" and
// "volume".
func TestTagManagement(t *testing.T) {
	gw := StartGateway(t)
	StartDaemonLite(t, gw)
	c := gw.EC2Client(t)

	vpcOut, err := c.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String("10.201.0.0/16")})
	require.NoError(t, err, "create-vpc")
	require.NotNil(t, vpcOut.Vpc, "create-vpc returned nil Vpc")
	vpcID := aws.StringValue(vpcOut.Vpc.VpcId)
	require.NotEmpty(t, vpcID, "VpcId empty")
	t.Cleanup(func() {
		_, _ = c.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
	})

	rtOut, err := c.DescribeRouteTables(&ec2.DescribeRouteTablesInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(vpcID)}},
			{Name: aws.String("association.main"), Values: []*string{aws.String("true")}},
		},
	})
	require.NoError(t, err, "describe-route-tables (main)")
	require.NotEmpty(t, rtOut.RouteTables, "no main route table found for vpc %s", vpcID)
	rtbID := aws.StringValue(rtOut.RouteTables[0].RouteTableId)
	require.NotEmpty(t, rtbID, "main RouteTableId empty")

	// Cleanup runs regardless of which sub-step failed so later assertions
	// see a tag-free vpc/route-table pair.
	t.Cleanup(func() {
		_, _ = c.DeleteTags(&ec2.DeleteTagsInput{
			Resources: []*string{aws.String(vpcID), aws.String(rtbID)},
			Tags: []*ec2.Tag{
				{Key: aws.String("Name")},
				{Key: aws.String("Environment")},
				{Key: aws.String("DeleteMe")},
			},
		})
	})

	// 6a: create three tags on the vpc (stand-in for "instance").
	_, err = c.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(vpcID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("e2e-test")},
			{Key: aws.String("Environment"), Value: aws.String("testing")},
			{Key: aws.String("DeleteMe"), Value: aws.String("please")},
		},
	})
	require.NoError(t, err, "create-tags vpc")

	// 6b: filter by resource-id should return all three.
	tags, err := describeTags(c, &ec2.Filter{
		Name:   aws.String("resource-id"),
		Values: []*string{aws.String(vpcID)},
	})
	require.NoError(t, err, "describe-tags resource-id")
	require.Lenf(t, tags, 3, "vpc tag count after 6a: %v", tagSummary(tags))

	// 6c: tag the route table (stand-in for "root volume") with two tags.
	_, err = c.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(rtbID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("e2e-root-vol")},
			{Key: aws.String("Environment"), Value: aws.String("testing")},
		},
	})
	require.NoError(t, err, "create-tags route table")

	// 6d: filter by key=Environment should return exactly 2 (one per resource).
	tags, err = describeTags(c, &ec2.Filter{
		Name:   aws.String("key"),
		Values: []*string{aws.String("Environment")},
	})
	require.NoError(t, err, "describe-tags key=Environment")
	require.Lenf(t, tags, 2, "Environment tag count across resources: %v", tagSummary(tags))

	// 6e: filter by resource-type=vpc should return 3 (all vpc tags).
	tags, err = describeTags(c, &ec2.Filter{
		Name:   aws.String("resource-type"),
		Values: []*string{aws.String("vpc")},
	})
	require.NoError(t, err, "describe-tags resource-type=vpc")
	require.Lenf(t, tags, 3, "vpc-typed tag count: %v", tagSummary(tags))

	// 6f: overwrite Name on the vpc — CreateTags with the same Key must
	// replace the prior Value.
	_, err = c.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(vpcID)},
		Tags: []*ec2.Tag{
			{Key: aws.String("Name"), Value: aws.String("e2e-test-updated")},
		},
	})
	require.NoError(t, err, "create-tags overwrite Name")

	tags, err = describeTags(c,
		&ec2.Filter{Name: aws.String("resource-id"), Values: []*string{aws.String(vpcID)}},
		&ec2.Filter{Name: aws.String("key"), Values: []*string{aws.String("Name")}},
	)
	require.NoError(t, err, "describe-tags resource-id + key=Name")
	require.Lenf(t, tags, 1, "expected exactly one Name tag on vpc: %v", tagSummary(tags))
	assert.Equal(t, "e2e-test-updated", aws.StringValue(tags[0].Value), "Name tag value after overwrite")

	// 6g: delete-tags with no Value drops the tag regardless of stored
	// value. VPC should be down to {Name, Environment}.
	_, err = c.DeleteTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(vpcID)},
		Tags:      []*ec2.Tag{{Key: aws.String("DeleteMe")}},
	})
	require.NoError(t, err, "delete-tags DeleteMe")

	tags, err = describeTags(c, &ec2.Filter{
		Name:   aws.String("resource-id"),
		Values: []*string{aws.String(vpcID)},
	})
	require.NoError(t, err, "describe-tags after unconditional delete")
	require.Lenf(t, tags, 2, "vpc tags after deleting DeleteMe: %v", tagSummary(tags))

	// 6h: delete-tags with a non-matching Value must be a no-op — the
	// Environment tag on the vpc still holds "testing".
	_, err = c.DeleteTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(vpcID)},
		Tags:      []*ec2.Tag{{Key: aws.String("Environment"), Value: aws.String("production")}},
	})
	require.NoError(t, err, "delete-tags Environment=production")

	tags, err = describeTags(c,
		&ec2.Filter{Name: aws.String("resource-id"), Values: []*string{aws.String(vpcID)}},
		&ec2.Filter{Name: aws.String("key"), Values: []*string{aws.String("Environment")}},
	)
	require.NoError(t, err, "describe-tags vpc Environment after no-op delete")
	require.Lenf(t, tags, 1, "Environment tag must survive non-matching delete: %v", tagSummary(tags))
	assert.Equal(t, "testing", aws.StringValue(tags[0].Value), "Environment value after no-op delete")

	// 6i: delete-tags with the correct Value removes the tag from the
	// targeted resource only. The route table's Environment tag must survive
	// because DeleteTags was scoped to the vpc.
	_, err = c.DeleteTags(&ec2.DeleteTagsInput{
		Resources: []*string{aws.String(vpcID)},
		Tags:      []*ec2.Tag{{Key: aws.String("Environment"), Value: aws.String("testing")}},
	})
	require.NoError(t, err, "delete-tags Environment=testing")

	tags, err = describeTags(c,
		&ec2.Filter{Name: aws.String("resource-id"), Values: []*string{aws.String(vpcID)}},
		&ec2.Filter{Name: aws.String("key"), Values: []*string{aws.String("Environment")}},
	)
	require.NoError(t, err, "describe-tags vpc Environment after match-delete")
	assert.Lenf(t, tags, 0, "Environment tag must be gone from vpc: %v", tagSummary(tags))

	tags, err = describeTags(c,
		&ec2.Filter{Name: aws.String("resource-id"), Values: []*string{aws.String(rtbID)}},
		&ec2.Filter{Name: aws.String("key"), Values: []*string{aws.String("Environment")}},
	)
	require.NoError(t, err, "describe-tags route table Environment after vpc-scoped delete")
	require.Lenf(t, tags, 1, "route table Environment tag must survive vpc-scoped delete: %v", tagSummary(tags))
	assert.Equal(t, rtbID, aws.StringValue(tags[0].ResourceId), "Environment tag still attached to main route table")

	// 6j: only Name should remain on the vpc.
	tags, err = describeTags(c, &ec2.Filter{
		Name:   aws.String("resource-id"),
		Values: []*string{aws.String(vpcID)},
	})
	require.NoError(t, err, "describe-tags final vpc")
	require.Lenf(t, tags, 1, "final vpc tag count: %v", tagSummary(tags))
	assert.Equal(t, "Name", aws.StringValue(tags[0].Key), "final remaining vpc tag key")
}

// describeTags is a thin wrapper that builds a DescribeTagsInput from the
// supplied filters — the test reaches for it once per sub-step so the
// assertion lines stay readable.
func describeTags(c *ec2.EC2, filters ...*ec2.Filter) ([]*ec2.TagDescription, error) {
	out, err := c.DescribeTags(&ec2.DescribeTagsInput{Filters: filters})
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
