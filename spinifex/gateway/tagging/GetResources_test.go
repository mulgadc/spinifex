package gateway_tagging

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	rgt "github.com/aws/aws-sdk-go/service/resourcegroupstaggingapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchesType(t *testing.T) {
	// Empty filter set admits everything.
	assert.True(t, matchesType(nil, "elasticloadbalancing:loadbalancer"))

	exact := map[string]bool{"elasticloadbalancing:loadbalancer": true}
	assert.True(t, matchesType(exact, "elasticloadbalancing:loadbalancer"))
	assert.False(t, matchesType(exact, "elasticloadbalancing:targetgroup"))
	assert.False(t, matchesType(exact, "ec2:subnet"))

	// Service-only filter admits all of that service's types.
	svcOnly := map[string]bool{"ec2": true}
	assert.True(t, matchesType(svcOnly, "ec2:subnet"))
	assert.True(t, matchesType(svcOnly, "ec2:security-group"))
	assert.False(t, matchesType(svcOnly, "elasticloadbalancing:loadbalancer"))
}

func TestEC2ARN(t *testing.T) {
	assert.Equal(t,
		"arn:aws:ec2:us-east-1:123456789012:subnet/subnet-abc",
		ec2ARN("us-east-1", "123456789012", "ec2:subnet", "subnet-abc"))
}

func mapping(arn string, kv ...string) *rgt.ResourceTagMapping {
	var tags []*rgt.Tag
	for i := 0; i+1 < len(kv); i += 2 {
		tags = append(tags, &rgt.Tag{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
	}
	return &rgt.ResourceTagMapping{ResourceARN: aws.String(arn), Tags: tags}
}

func tagFilter(key string, values ...string) *rgt.TagFilter {
	return &rgt.TagFilter{Key: aws.String(key), Values: aws.StringSlice(values)}
}

func TestFilterByTags(t *testing.T) {
	in := []*rgt.ResourceTagMapping{
		mapping("arn:a", "elbv2.k8s.aws/cluster", "prod", "env", "live"),
		mapping("arn:b", "elbv2.k8s.aws/cluster", "dev"),
		mapping("arn:c", "env", "live"),
	}

	// No filters: passthrough.
	assert.Len(t, filterByTags(in, nil), 3)

	// Key-only filter: any value for the key.
	got := filterByTags(in, []*rgt.TagFilter{tagFilter("elbv2.k8s.aws/cluster")})
	require.Len(t, got, 2)

	// Key+value filter: exact value match (OR over values).
	got = filterByTags(in, []*rgt.TagFilter{tagFilter("elbv2.k8s.aws/cluster", "prod", "staging")})
	require.Len(t, got, 1)
	assert.Equal(t, "arn:a", aws.StringValue(got[0].ResourceARN))

	// AND across filters.
	got = filterByTags(in, []*rgt.TagFilter{
		tagFilter("elbv2.k8s.aws/cluster", "prod"),
		tagFilter("env", "live"),
	})
	require.Len(t, got, 1)
	assert.Equal(t, "arn:a", aws.StringValue(got[0].ResourceARN))

	// Filter on a key no resource has: empty result.
	assert.Empty(t, filterByTags(in, []*rgt.TagFilter{tagFilter("missing")}))
}

func TestPaginate(t *testing.T) {
	all := []*rgt.ResourceTagMapping{
		mapping("arn:0"), mapping("arn:1"), mapping("arn:2"), mapping("arn:3"), mapping("arn:4"),
	}

	// First page of 2, expect a next token.
	page, next, err := paginate(all, nil, aws.Int64(2))
	require.NoError(t, err)
	assert.Len(t, page, 2)
	assert.Equal(t, "2", next)

	// Follow the token to the middle page.
	page, next, err = paginate(all, aws.String("2"), aws.Int64(2))
	require.NoError(t, err)
	assert.Len(t, page, 2)
	assert.Equal(t, "4", next)

	// Last partial page: no next token.
	page, next, err = paginate(all, aws.String("4"), aws.Int64(2))
	require.NoError(t, err)
	assert.Len(t, page, 1)
	assert.Equal(t, "", next)

	// Default page size returns everything in one page.
	page, next, err = paginate(all, nil, nil)
	require.NoError(t, err)
	assert.Len(t, page, 5)
	assert.Equal(t, "", next)

	// Bad token is rejected.
	_, _, err = paginate(all, aws.String("not-a-number"), nil)
	require.Error(t, err)
}

func TestTagMapToRGT_SortedDeterministic(t *testing.T) {
	out := tagMapToRGT(map[string]string{"b": "2", "a": "1", "c": "3"})
	require.Len(t, out, 3)
	assert.Equal(t, "a", aws.StringValue(out[0].Key))
	assert.Equal(t, "b", aws.StringValue(out[1].Key))
	assert.Equal(t, "c", aws.StringValue(out[2].Key))
}
