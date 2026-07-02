package handlers_ec2_placementgroup

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTaggedGroup(t *testing.T, svc *PlacementGroupServiceImpl, name string, tags map[string]string) *ec2.PlacementGroup {
	t.Helper()
	var ec2Tags []*ec2.Tag
	for k, v := range tags {
		ec2Tags = append(ec2Tags, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	out, err := svc.CreatePlacementGroup(&ec2.CreatePlacementGroupInput{
		GroupName: aws.String(name),
		Strategy:  aws.String("spread"),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("placement-group"),
			Tags:         ec2Tags,
		}},
	}, testAccountID)
	require.NoError(t, err)
	return out.PlacementGroup
}

func describeWithFilter(t *testing.T, svc *PlacementGroupServiceImpl, name, value string) []*ec2.PlacementGroup {
	t.Helper()
	out, err := svc.DescribePlacementGroups(&ec2.DescribePlacementGroupsInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String(name),
			Values: []*string{aws.String(value)},
		}},
	}, testAccountID)
	require.NoError(t, err)
	return out.PlacementGroups
}

func TestCreatePlacementGroup_TagSpecifications(t *testing.T) {
	svc := setupTestService(t)
	pg := createTaggedGroup(t, svc, "tagged-group", map[string]string{"Name": "web", "env": "dev"})

	tags := filterutil.EC2TagsToMap(pg.Tags)
	assert.Equal(t, "web", tags["Name"])
	assert.Equal(t, "dev", tags["env"])

	out, err := svc.DescribePlacementGroups(&ec2.DescribePlacementGroupsInput{
		GroupNames: []*string{aws.String("tagged-group")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.PlacementGroups, 1)
	descTags := filterutil.EC2TagsToMap(out.PlacementGroups[0].Tags)
	assert.Equal(t, "web", descTags["Name"])
}

func TestCreatePlacementGroup_TagSpecificationsWrongType(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreatePlacementGroup(&ec2.CreatePlacementGroupInput{
		GroupName: aws.String("untagged-group"),
		Strategy:  aws.String("cluster"),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("volume"),
			Tags:         []*ec2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
		}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.PlacementGroup.Tags)
}

func TestDescribePlacementGroups_TagFilters(t *testing.T) {
	svc := setupTestService(t)
	createTaggedGroup(t, svc, "pg-dev", map[string]string{"env": "dev"})
	createTaggedGroup(t, svc, "pg-prod", map[string]string{"env": "prod"})

	groups := describeWithFilter(t, svc, "tag:env", "dev")
	require.Len(t, groups, 1)
	assert.Equal(t, "pg-dev", *groups[0].GroupName)

	groups = describeWithFilter(t, svc, "tag-key", "env")
	assert.Len(t, groups, 2)

	groups = describeWithFilter(t, svc, "tag-value", "prod")
	require.Len(t, groups, 1)
	assert.Equal(t, "pg-prod", *groups[0].GroupName)

	groups = describeWithFilter(t, svc, "tag:env", "staging")
	assert.Empty(t, groups)

	groups = describeWithFilter(t, svc, "tag-key", "missing")
	assert.Empty(t, groups)
}
