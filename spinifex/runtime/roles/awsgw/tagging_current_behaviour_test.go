//test:in-package — drives GetResources through newTaggingAggregator, the
// composition root's own wiring, which is unexported.

package awsgw

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
)

// The TestCurrentBehaviour_Tagging_GetResources tests pin today's Resource
// Groups Tagging API outcomes through the aggregator AWSGW wires. They run the
// real EC2 tag service on an embedded JetStream, and describe current
// behaviour, not the target design. The daemon's record-mirror fan-out is not
// wired here, so only the central tag index is exercised.

const (
	authzAccountID = "123456789012"
	authzRegion    = "ap-southeast-2"
)

// newTaggingCBAggregator serves the EC2 tag subjects from a real
// TagsServiceImpl over a fresh JetStream bucket, and returns AWSGW's
// GetResources wiring with the NATS client that writes tags as the gateway does.
func newTaggingCBAggregator(t *testing.T) (*tagging.Aggregator, ec2tags.TagsService) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := ec2tags.GetOrCreateTagsBucket(t.Context(), js)
	require.NoError(t, err)
	svc := ec2tags.NewTagsServiceImplWithStore(&config.Config{}, nil, kv)

	serve := func(subject string, fn func(*nats.Msg)) {
		sub, err := nc.Subscribe(subject, fn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	serve("ec2.CreateTags", func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, in *ec2.CreateTagsInput) (*ec2.CreateTagsOutput, error) {
			return svc.CreateTags(ctx, in, natsmsg.AccountIDFromMsg(msg))
		})
	})
	serve("ec2.DeleteTags", func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, in *ec2.DeleteTagsInput) (*ec2.DeleteTagsOutput, error) {
			return svc.DeleteTags(ctx, in, natsmsg.AccountIDFromMsg(msg))
		})
	})
	serve("ec2.DescribeTags", func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, in *ec2.DescribeTagsInput) (*ec2.DescribeTagsOutput, error) {
			return svc.DescribeTags(ctx, in, natsmsg.AccountIDFromMsg(msg))
		})
	})

	return newTaggingAggregator(nc, authzRegion), ec2tags.NewNATSTagsService(nc)
}

// createTags writes tags over the ec2.CreateTags subject the gateway's EC2
// CreateTags dispatch uses.
func createTags(t *testing.T, tags ec2tags.TagsService, resources []string, kv map[string]string) {
	t.Helper()
	in := &ec2.CreateTagsInput{Resources: aws.StringSlice(resources)}
	for k, v := range kv {
		in.Tags = append(in.Tags, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	_, err := tags.CreateTags(t.Context(), in, authzAccountID)
	require.NoError(t, err)
}

// deleteAllTags sends DeleteTags with no Tag list, which clears every tag.
func deleteAllTags(t *testing.T, tags ec2tags.TagsService, resource string) {
	t.Helper()
	_, err := tags.DeleteTags(t.Context(), &ec2.DeleteTagsInput{Resources: aws.StringSlice([]string{resource})}, authzAccountID)
	require.NoError(t, err)
}

type getResourcesPage struct {
	ResourceTagMappingList []struct {
		ResourceARN string `json:"ResourceARN"`
		Tags        []struct {
			Key   string `json:"Key"`
			Value string `json:"Value"`
		} `json:"Tags"`
	} `json:"ResourceTagMappingList"`
	PaginationToken string `json:"PaginationToken"`
}

// getResources runs GetResources for the test account and decodes the body the
// gateway would write for it.
func getResources(t *testing.T, agg *tagging.Aggregator, body string) (getResourcesPage, error) {
	t.Helper()
	out, err := agg.GetResources(t.Context(), authzAccountID, []byte(body))
	if err != nil {
		return getResourcesPage{}, err
	}
	w := httptest.NewRecorder()
	tagging.WriteJSONResponse(w, out)
	var page getResourcesPage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	return page, nil
}

func pageARNs(p getResourcesPage) []string {
	out := make([]string, 0, len(p.ResourceTagMappingList))
	for _, m := range p.ResourceTagMappingList {
		out = append(out, m.ResourceARN)
	}
	return out
}

// GetResources lists whatever the central index holds: a well-formed ID naming
// no resource is reported as that resource. An ID with no recognised prefix
// never reaches the index, because CreateTags rejects it as InvalidID.
func TestCurrentBehaviour_Tagging_GetResourcesListsUnresolvedEC2IDs(t *testing.T) {
	agg, tags := newTaggingCBAggregator(t)

	_, err := tags.CreateTags(t.Context(), &ec2.CreateTagsInput{
		Resources: aws.StringSlice([]string{"not-a-real-id"}),
		Tags:      []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, authzAccountID)
	require.EqualError(t, err, awserrors.ErrorInvalidID)
	createTags(t, tags, []string{"vol-00000000000000000"}, map[string]string{"env": "prod"})

	page, err := getResources(t, agg, `{"ResourceTypeFilters":["ec2"]}`)
	require.NoError(t, err)
	prefix := "arn:aws:ec2:" + authzRegion + ":" + authzAccountID + ":"
	assert.ElementsMatch(t, []string{
		prefix + "volume/vol-00000000000000000",
	}, pageARNs(page))
}

// DeleteTags with no Tag list clears every tag, and the resource then drops
// out of GetResources even though it was tagged before.
func TestCurrentBehaviour_Tagging_GetResourcesOmitsResourceWhoseTagsWereAllRemoved(t *testing.T) {
	agg, tags := newTaggingCBAggregator(t)
	const sg = "sg-0a1b2c3d4e5f60718"

	createTags(t, tags, []string{sg}, map[string]string{"env": "prod"})
	page, err := getResources(t, agg, `{"ResourceTypeFilters":["ec2"]}`)
	require.NoError(t, err)
	require.Len(t, page.ResourceTagMappingList, 1)

	deleteAllTags(t, tags, sg)
	page, err = getResources(t, agg, `{"ResourceTypeFilters":["ec2"]}`)
	require.NoError(t, err)
	assert.Empty(t, page.ResourceTagMappingList)
}

// The pagination token is an offset into the result set recomputed on every
// call. A resource tagged between two page reads that sorts ahead of the
// offset shifts the set, so the second page repeats the first page's resource.
func TestCurrentBehaviour_Tagging_GetResourcesOffsetTokenRepeatsAfterInterleavedWrite(t *testing.T) {
	agg, tags := newTaggingCBAggregator(t)

	createTags(t, tags, []string{"sg-0000000000000000b", "sg-0000000000000000c"}, map[string]string{"env": "prod"})

	first, err := getResources(t, agg, `{"ResourceTypeFilters":["ec2"],"ResourcesPerPage":1}`)
	require.NoError(t, err)
	require.Len(t, first.ResourceTagMappingList, 1)
	require.Equal(t, "1", first.PaginationToken)

	createTags(t, tags, []string{"sg-0000000000000000a"}, map[string]string{"env": "prod"})

	second, err := getResources(t, agg, `{"ResourceTypeFilters":["ec2"],"ResourcesPerPage":1,"PaginationToken":"1"}`)
	require.NoError(t, err)
	require.Len(t, second.ResourceTagMappingList, 1)
	assert.Equal(t, pageARNs(first), pageARNs(second), "the second page repeats the first")
}

// A token that is not a non-negative integer is InvalidParameterValue, and a
// page size above 100 is accepted rather than refused.
func TestCurrentBehaviour_Tagging_GetResourcesPaginationValidation(t *testing.T) {
	agg, tags := newTaggingCBAggregator(t)
	createTags(t, tags, []string{"sg-0000000000000000a"}, map[string]string{"env": "prod"})

	_, err := getResources(t, agg, `{"PaginationToken":"not-a-token","ResourceTypeFilters":["ec2"]}`)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())

	page, err := getResources(t, agg, `{"ResourcesPerPage":500,"ResourceTypeFilters":["ec2"]}`)
	require.NoError(t, err)
	assert.Len(t, page.ResourceTagMappingList, 1)
}

// GetResources aggregates only the EC2 central index and ELBv2 load balancers
// and target groups. A type filter naming any other service returns an empty
// list without error, whatever that service holds.
func TestCurrentBehaviour_Tagging_GetResourcesAggregatesOnlyEC2AndELBv2(t *testing.T) {
	agg, tags := newTaggingCBAggregator(t)
	createTags(t, tags, []string{"sg-0000000000000000a"}, map[string]string{"env": "prod"})

	for _, filter := range []string{"rds", "ecs:cluster", "eks:cluster", "ecr:repository", "acm:certificate", "iam:role", "elasticloadbalancing:listener"} {
		t.Run(filter, func(t *testing.T) {
			page, err := getResources(t, agg, `{"ResourceTypeFilters":["`+filter+`"]}`)
			require.NoError(t, err)
			assert.Empty(t, page.ResourceTagMappingList)
		})
	}
}
