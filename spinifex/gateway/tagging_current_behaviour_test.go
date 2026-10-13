//test:in-package — drives EC2_Request through the gateway's unexported test
// helper setupEC2Request.

package gateway

import (
	"context"
	"fmt"
	"net/http"
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

// The TestCurrentBehaviour_Tagging_ tests here pin today's EC2 tag-write
// outcomes at the AWSGW door. They drive the real router over the real EC2 tag
// service on an embedded JetStream, and describe current behaviour, not the
// target design. The daemon's record-mirror fan-out is not wired here, so only
// the central tag index is exercised.

// newTaggingCBGateway serves ec2.CreateTags from a real TagsServiceImpl
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
