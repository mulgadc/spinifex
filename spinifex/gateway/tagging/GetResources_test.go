package gateway_tagging

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	rgt "github.com/aws/aws-sdk-go/service/resourcegroupstaggingapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLister returns canned mappings so getResources can be exercised without a
// NATS backend.
type fakeLister struct {
	elbv2     []*rgt.ResourceTagMapping
	ec2       []*rgt.ResourceTagMapping
	elbv2Err  error
	ec2Err    error
	gotFilter map[string]bool
}

func (f *fakeLister) listELBv2(typeFilters map[string]bool) ([]*rgt.ResourceTagMapping, error) {
	f.gotFilter = typeFilters
	return f.elbv2, f.elbv2Err
}

func (f *fakeLister) listEC2(typeFilters map[string]bool) ([]*rgt.ResourceTagMapping, error) {
	return f.ec2, f.ec2Err
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

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

func TestGetResources_MergesFiltersSortsPaginates(t *testing.T) {
	lister := &fakeLister{
		elbv2: []*rgt.ResourceTagMapping{
			mapping("arn:elb:c", "elbv2.k8s.aws/cluster", "prod"),
			mapping("arn:elb:a", "elbv2.k8s.aws/cluster", "dev"),
		},
		ec2: []*rgt.ResourceTagMapping{
			mapping("arn:ec2:b", "elbv2.k8s.aws/cluster", "prod"),
		},
	}

	// Tag-filter to prod + page size 1: two matches (arn:ec2:b, arn:elb:c),
	// ARN-sorted, first page returns arn:ec2:b with a next token.
	body := mustJSON(t, rgt.GetResourcesInput{
		TagFilters:       []*rgt.TagFilter{tagFilter("elbv2.k8s.aws/cluster", "prod")},
		ResourcesPerPage: aws.Int64(1),
	})
	out, err := getResources(lister, body)
	require.NoError(t, err)
	res, ok := out.(*rgt.GetResourcesOutput)
	require.True(t, ok)
	require.Len(t, res.ResourceTagMappingList, 1)
	assert.Equal(t, "arn:ec2:b", aws.StringValue(res.ResourceTagMappingList[0].ResourceARN))
	assert.Equal(t, "1", aws.StringValue(res.PaginationToken))

	// Follow the token: last page, arn:elb:c, empty token.
	body = mustJSON(t, rgt.GetResourcesInput{
		TagFilters:       []*rgt.TagFilter{tagFilter("elbv2.k8s.aws/cluster", "prod")},
		ResourcesPerPage: aws.Int64(1),
		PaginationToken:  aws.String("1"),
	})
	out, err = getResources(lister, body)
	require.NoError(t, err)
	res, ok = out.(*rgt.GetResourcesOutput)
	require.True(t, ok)
	require.Len(t, res.ResourceTagMappingList, 1)
	assert.Equal(t, "arn:elb:c", aws.StringValue(res.ResourceTagMappingList[0].ResourceARN))
	assert.Equal(t, "", aws.StringValue(res.PaginationToken))
}

func TestGetResources_ResourceTypeFiltersLowercasedAndPassed(t *testing.T) {
	lister := &fakeLister{}
	body := mustJSON(t, rgt.GetResourcesInput{
		ResourceTypeFilters: aws.StringSlice([]string{"ElasticLoadBalancing:LoadBalancer"}),
	})
	_, err := getResources(lister, body)
	require.NoError(t, err)
	assert.True(t, lister.gotFilter["elasticloadbalancing:loadbalancer"])
}

func TestGetResources_ListerErrorPropagates(t *testing.T) {
	lister := &fakeLister{elbv2Err: assert.AnError}
	_, err := getResources(lister, []byte("{}"))
	require.ErrorIs(t, err, assert.AnError)
}

func TestGetResources_InvalidBody(t *testing.T) {
	_, err := getResources(&fakeLister{}, []byte("not-json"))
	require.Error(t, err)
}

func TestBuildEC2Mappings(t *testing.T) {
	tags := []*ec2.TagDescription{
		{ResourceId: aws.String("subnet-1"), ResourceType: aws.String("subnet"), Key: aws.String("Name"), Value: aws.String("a")},
		{ResourceId: aws.String("subnet-1"), ResourceType: aws.String("subnet"), Key: aws.String("env"), Value: aws.String("prod")},
		{ResourceId: aws.String("sg-1"), ResourceType: aws.String("security-group"), Key: aws.String("Name"), Value: aws.String("b")},
		{ResourceId: aws.String(""), ResourceType: aws.String("subnet"), Key: aws.String("skip"), Value: aws.String("me")},
	}

	// No type filter: both resources, tags merged per resource.
	all := buildEC2Mappings(tags, "us-east-1", "123456789012", nil)
	require.Len(t, all, 2)
	bySubnet := map[string]*rgt.ResourceTagMapping{}
	for _, m := range all {
		bySubnet[aws.StringValue(m.ResourceARN)] = m
	}
	subnet := bySubnet["arn:aws:ec2:us-east-1:123456789012:subnet/subnet-1"]
	require.NotNil(t, subnet)
	assert.Len(t, subnet.Tags, 2)

	// Type filter narrows to subnets only.
	onlySubnets := buildEC2Mappings(tags, "us-east-1", "123456789012", map[string]bool{"ec2:subnet": true})
	require.Len(t, onlySubnets, 1)
	assert.Equal(t, "arn:aws:ec2:us-east-1:123456789012:subnet/subnet-1", aws.StringValue(onlySubnets[0].ResourceARN))
}

func TestTagMapToRGT_SortedDeterministic(t *testing.T) {
	out := tagMapToRGT(map[string]string{"b": "2", "a": "1", "c": "3"})
	require.Len(t, out, 3)
	assert.Equal(t, "a", aws.StringValue(out[0].Key))
	assert.Equal(t, "b", aws.StringValue(out[1].Key))
	assert.Equal(t, "c", aws.StringValue(out[2].Key))
}
