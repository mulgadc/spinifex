//test:in-package — filterByTags, paginate and tagMapToRGT are unexported
// helpers of the aggregator and are tested directly.

package tagging

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	rgt "github.com/aws/aws-sdk-go/service/resourcegroupstaggingapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

const testAccountID = "123456789012"

// fakeLister returns canned resources so GetResources can be exercised without
// a NATS backend.
type fakeLister struct {
	out       []TaggedResource
	err       error
	gotFilter map[string]bool
}

var _ TaggedLister = (*fakeLister)(nil)

func (f *fakeLister) ListTagged(_ context.Context, _ string, typeFilters map[string]bool) ([]TaggedResource, error) {
	f.gotFilter = typeFilters
	return f.out, f.err
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
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
	assert.Empty(t, next)

	// Default page size returns everything in one page.
	page, next, err = paginate(all, nil, nil)
	require.NoError(t, err)
	assert.Len(t, page, 5)
	assert.Empty(t, next)

	// The page-size bounds themselves are accepted.
	page, next, err = paginate(all, nil, aws.Int64(1))
	require.NoError(t, err)
	assert.Len(t, page, 1)
	assert.Equal(t, "1", next)

	page, next, err = paginate(all, nil, aws.Int64(100))
	require.NoError(t, err)
	assert.Len(t, page, 5)
	assert.Empty(t, next)
}

// AWS accepts ResourcesPerPage of 1 to 100 and reports out-of-range or
// malformed pagination input as InvalidParameterException.
func TestPaginate_RejectsInvalidInputAsInvalidParameterException(t *testing.T) {
	all := []*rgt.ResourceTagMapping{mapping("arn:0"), mapping("arn:1")}

	cases := map[string]struct {
		token   *string
		perPage *int64
	}{
		"zero page size":             {perPage: aws.Int64(0)},
		"negative page size":         {perPage: aws.Int64(-1)},
		"page size above 100":        {perPage: aws.Int64(101)},
		"max int64 page with offset": {token: aws.String("1"), perPage: aws.Int64(math.MaxInt64)},
		"non-numeric token":          {token: aws.String("not-a-number")},
		"negative token":             {token: aws.String("-1")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := paginate(all, tc.token, tc.perPage)
			code, ok := awserrors.ResolveErrorCode(err)
			require.True(t, ok, "error %v carries no AWS code", err)
			assert.Equal(t, "InvalidParameterException", code)
		})
	}
}
func TestGetResources_MergesFiltersSortsPaginates(t *testing.T) {
	elbv2 := &fakeLister{out: []TaggedResource{
		{ARN: "arn:elb:c", Tags: map[string]string{"elbv2.k8s.aws/cluster": "prod"}},
		{ARN: "arn:elb:a", Tags: map[string]string{"elbv2.k8s.aws/cluster": "dev"}},
	}}
	ec2 := &fakeLister{out: []TaggedResource{
		{ARN: "arn:ec2:b", Tags: map[string]string{"elbv2.k8s.aws/cluster": "prod"}},
	}}
	agg := NewAggregator(elbv2, ec2)

	// Tag-filter to prod + page size 1: two matches (arn:ec2:b, arn:elb:c),
	// ARN-sorted, first page returns arn:ec2:b with a next token.
	body := mustJSON(t, rgt.GetResourcesInput{
		TagFilters:       []*rgt.TagFilter{tagFilter("elbv2.k8s.aws/cluster", "prod")},
		ResourcesPerPage: aws.Int64(1),
	})
	out, err := agg.GetResources(context.Background(), testAccountID, body)
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
	out, err = agg.GetResources(context.Background(), testAccountID, body)
	require.NoError(t, err)
	res, ok = out.(*rgt.GetResourcesOutput)
	require.True(t, ok)
	require.Len(t, res.ResourceTagMappingList, 1)
	assert.Equal(t, "arn:elb:c", aws.StringValue(res.ResourceTagMappingList[0].ResourceARN))
	assert.Empty(t, aws.StringValue(res.PaginationToken))
}

func TestGetResources_ResourceTypeFiltersLowercasedAndPassed(t *testing.T) {
	lister := &fakeLister{}
	body := mustJSON(t, rgt.GetResourcesInput{
		ResourceTypeFilters: aws.StringSlice([]string{"ElasticLoadBalancing:LoadBalancer"}),
	})
	_, err := NewAggregator(lister).GetResources(context.Background(), testAccountID, body)
	require.NoError(t, err)
	assert.True(t, lister.gotFilter["elasticloadbalancing:loadbalancer"])
}

func TestGetResources_ListerErrorPropagates(t *testing.T) {
	agg := NewAggregator(&fakeLister{err: assert.AnError}, &fakeLister{})
	_, err := agg.GetResources(context.Background(), testAccountID, []byte("{}"))
	require.ErrorIs(t, err, assert.AnError)
}

func TestGetResources_InvalidBody(t *testing.T) {
	_, err := NewAggregator(&fakeLister{}).GetResources(context.Background(), testAccountID, []byte("not-json"))
	require.Error(t, err)
}

func TestTagMapToRGT_SortedDeterministic(t *testing.T) {
	out := tagMapToRGT(map[string]string{"b": "2", "a": "1", "c": "3"})
	require.Len(t, out, 3)
	assert.Equal(t, "a", aws.StringValue(out[0].Key))
	assert.Equal(t, "b", aws.StringValue(out[1].Key))
	assert.Equal(t, "c", aws.StringValue(out[2].Key))
}
