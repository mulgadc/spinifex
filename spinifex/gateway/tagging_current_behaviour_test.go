//test:in-package — drives EC2_Request and Tagging_Request through the
// gateway's unexported test helpers (setupEC2Request, setupTaggingRequest).

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
)

// The TestCurrentBehaviour_Tagging_ tests pin today's EC2 tag-write and
// Resource Groups Tagging API outcomes at the AWSGW door. They drive the real
// router over the real EC2 tag service on an embedded JetStream, and describe
// current behaviour, not the target design. The daemon's record-mirror fan-out
// is not wired here, so only the central tag index is exercised.

// newTaggingCBGateway serves the EC2 tag subjects from a real TagsServiceImpl
// over a fresh JetStream bucket, with no legacy object store.
func newTaggingCBGateway(t *testing.T) (*GatewayConfig, *ec2tags.TagsServiceImpl) {
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

	gw := &GatewayConfig{DisableLogging: true, Region: authzRegion, IAMService: allowAllIAMService(), NATSConn: nc}
	return gw, svc
}

// ec2TagQuery builds a CreateTags or DeleteTags query body.
func ec2TagQuery(action string, resources []string, tags map[string]string) string {
	q := url.Values{"Action": {action}}
	for i, r := range resources {
		q.Set(fmt.Sprintf("ResourceId.%d", i+1), r)
	}
	i := 0
	for k, v := range tags {
		i++
		q.Set(fmt.Sprintf("Tag.%d.Key", i), k)
		q.Set(fmt.Sprintf("Tag.%d.Value", i), v)
	}
	return q.Encode()
}

func requireEC2OK(t *testing.T, gw *GatewayConfig, body string) {
	t.Helper()
	w := serveEC2(gw, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
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

func getResources(t *testing.T, gw *GatewayConfig, body string) (getResourcesPage, error) {
	t.Helper()
	w := httptest.NewRecorder()
	err := gw.Tagging_Request(w, setupTaggingRequest("ResourceGroupsTaggingAPI_20170126.GetResources", body))
	if err != nil {
		return getResourcesPage{}, err
	}
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

func centralTagCount(t *testing.T, svc *ec2tags.TagsServiceImpl, resourceID string) int {
	t.Helper()
	out, err := svc.DescribeTags(t.Context(), &ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{{Name: aws.String("resource-id"), Values: []*string{aws.String(resourceID)}}},
	}, authzAccountID)
	require.NoError(t, err)
	return len(out.Tags)
}

// CreateTags refuses an aws: key but applies no tag-count, key-length or
// value-length limit: 51 tags, a 129-character key and a 257-character value
// are all stored.
func TestCurrentBehaviour_Tagging_EC2CreateTagsAppliesNoLengthOrCountLimits(t *testing.T) {
	gw, svc := newTaggingCBGateway(t)
	const sg = "sg-0a1b2c3d4e5f60718"

	tags := map[string]string{strings.Repeat("k", 129): strings.Repeat("v", 257)}
	for i := range 50 {
		tags[fmt.Sprintf("key-%02d", i)] = "v"
	}
	requireEC2OK(t, gw, ec2TagQuery("CreateTags", []string{sg}, tags))
	assert.Equal(t, 51, centralTagCount(t, svc, sg))

	w := serveEC2(gw, ec2TagQuery("CreateTags", []string{sg}, map[string]string{"aws:reserved": "v"}))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "<Code>"+awserrors.ErrorInvalidParameterValue+"</Code>")
}

// GetResources lists whatever the central index holds: an ID with no
// recognised prefix is reported with resource type "unknown", and a
// well-formed ID naming no resource is reported as that resource.
func TestCurrentBehaviour_Tagging_GetResourcesListsUnresolvedEC2IDs(t *testing.T) {
	gw, _ := newTaggingCBGateway(t)

	requireEC2OK(t, gw, ec2TagQuery("CreateTags", []string{"not-a-real-id", "vol-00000000000000000"}, map[string]string{"env": "prod"}))

	page, err := getResources(t, gw, `{"ResourceTypeFilters":["ec2"]}`)
	require.NoError(t, err)
	prefix := "arn:aws:ec2:" + authzRegion + ":" + authzAccountID + ":"
	assert.ElementsMatch(t, []string{
		prefix + "unknown/not-a-real-id",
		prefix + "volume/vol-00000000000000000",
	}, pageARNs(page))
}

// DeleteTags with no Tag list clears every tag, and the resource then drops
// out of GetResources even though it was tagged before.
func TestCurrentBehaviour_Tagging_GetResourcesOmitsResourceWhoseTagsWereAllRemoved(t *testing.T) {
	gw, _ := newTaggingCBGateway(t)
	const sg = "sg-0a1b2c3d4e5f60718"

	requireEC2OK(t, gw, ec2TagQuery("CreateTags", []string{sg}, map[string]string{"env": "prod"}))
	page, err := getResources(t, gw, `{"ResourceTypeFilters":["ec2"]}`)
	require.NoError(t, err)
	require.Len(t, page.ResourceTagMappingList, 1)

	requireEC2OK(t, gw, ec2TagQuery("DeleteTags", []string{sg}, nil))
	page, err = getResources(t, gw, `{"ResourceTypeFilters":["ec2"]}`)
	require.NoError(t, err)
	assert.Empty(t, page.ResourceTagMappingList)
}

// The pagination token is an offset into the result set recomputed on every
// call. A resource tagged between two page reads that sorts ahead of the
// offset shifts the set, so the second page repeats the first page's resource.
func TestCurrentBehaviour_Tagging_GetResourcesOffsetTokenRepeatsAfterInterleavedWrite(t *testing.T) {
	gw, _ := newTaggingCBGateway(t)

	requireEC2OK(t, gw, ec2TagQuery("CreateTags", []string{"sg-0000000000000000b", "sg-0000000000000000c"}, map[string]string{"env": "prod"}))

	first, err := getResources(t, gw, `{"ResourceTypeFilters":["ec2"],"ResourcesPerPage":1}`)
	require.NoError(t, err)
	require.Len(t, first.ResourceTagMappingList, 1)
	require.Equal(t, "1", first.PaginationToken)

	requireEC2OK(t, gw, ec2TagQuery("CreateTags", []string{"sg-0000000000000000a"}, map[string]string{"env": "prod"}))

	second, err := getResources(t, gw, `{"ResourceTypeFilters":["ec2"],"ResourcesPerPage":1,"PaginationToken":"1"}`)
	require.NoError(t, err)
	require.Len(t, second.ResourceTagMappingList, 1)
	assert.Equal(t, pageARNs(first), pageARNs(second), "the second page repeats the first")
}

// A token that is not a non-negative integer is InvalidParameterValue, and a
// page size above 100 is accepted rather than refused.
func TestCurrentBehaviour_Tagging_GetResourcesPaginationValidation(t *testing.T) {
	gw, _ := newTaggingCBGateway(t)
	requireEC2OK(t, gw, ec2TagQuery("CreateTags", []string{"sg-0000000000000000a"}, map[string]string{"env": "prod"}))

	_, err := getResources(t, gw, `{"PaginationToken":"not-a-token","ResourceTypeFilters":["ec2"]}`)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())

	page, err := getResources(t, gw, `{"ResourcesPerPage":500,"ResourceTypeFilters":["ec2"]}`)
	require.NoError(t, err)
	assert.Len(t, page.ResourceTagMappingList, 1)
}

// GetResources aggregates only the EC2 central index and ELBv2 load balancers
// and target groups. A type filter naming any other service returns an empty
// list without error, whatever that service holds.
func TestCurrentBehaviour_Tagging_GetResourcesAggregatesOnlyEC2AndELBv2(t *testing.T) {
	gw, _ := newTaggingCBGateway(t)
	requireEC2OK(t, gw, ec2TagQuery("CreateTags", []string{"sg-0000000000000000a"}, map[string]string{"env": "prod"}))

	for _, filter := range []string{"rds", "ecs:cluster", "eks:cluster", "ecr:repository", "acm:certificate", "iam:role", "elasticloadbalancing:listener"} {
		t.Run(filter, func(t *testing.T) {
			page, err := getResources(t, gw, `{"ResourceTypeFilters":["`+filter+`"]}`)
			require.NoError(t, err)
			assert.Empty(t, page.ResourceTagMappingList)
		})
	}
}
