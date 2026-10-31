package gateway

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const listTarget = "AmazonSQS.ListQueues"

func (a sqsApp) urlOf(name string) string { return a.url + "/" + ecrTestAccount + "/" + name }

// seedElsewhere creates same-named queues in another account and another
// Region, which the caller's listing must never show.
func (a sqsApp) seedElsewhere(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	for _, scope := range [][2]string{{otherOwner, testRegion}, {ecrTestAccount, "ap-southeast-2"}} {
		_, err := a.store.Create(ctx, queue.CreateRequest{Account: scope[0], Region: scope[1], Name: name})
		require.NoError(t, err)
	}
}

func TestSQSListQueues_SDK(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()
	for _, n := range []string{"orders", "orders-dlq", "billing"} {
		a.create(t, n, nil)
	}
	a.seedElsewhere(t, "orders-other")
	gone := a.create(t, "orders-gone", nil)
	_, err := a.store.MarkDeleting(ctx, ecrTestAccount, testRegion, "orders-gone")
	require.NoError(t, err)

	out, err := a.client().ListQueues(ctx, &sqs.ListQueuesInput{})
	require.NoError(t, err)
	assert.Equal(t, []string{a.urlOf("billing"), a.urlOf("orders"), a.urlOf("orders-dlq")}, out.QueueUrls,
		"only active queues in the caller's account and Region")
	assert.Nil(t, out.NextToken)
	assert.NotContains(t, out.QueueUrls, gone)

	out, err = a.client().ListQueues(ctx, &sqs.ListQueuesInput{QueueNamePrefix: aws.String("orders")})
	require.NoError(t, err)
	assert.Equal(t, []string{a.urlOf("orders"), a.urlOf("orders-dlq")}, out.QueueUrls)

	out, err = a.client().ListQueues(ctx, &sqs.ListQueuesInput{QueueNamePrefix: aws.String("Orders")})
	require.NoError(t, err)
	assert.Empty(t, out.QueueUrls, "the prefix is case-sensitive")

	// Every queue URL the listing renders resolves through GetQueueAttributes.
	for _, u := range []string{a.urlOf("orders"), a.urlOf("billing")} {
		_, err := a.client().GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(u)})
		require.NoError(t, err, u)
	}
}

func TestSQSListQueues_Pagination(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()
	for _, n := range []string{"q1", "q2", "q3", "q4", "q5"} {
		a.create(t, n, nil)
	}

	var got []string
	in := &sqs.ListQueuesInput{MaxResults: aws.Int32(2)}
	pages := 0
	for p := sqs.NewListQueuesPaginator(a.client(), in); p.HasMorePages(); {
		out, err := p.NextPage(ctx)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(out.QueueUrls), 2)
		got = append(got, out.QueueUrls...)
		pages++
	}
	assert.Equal(t, []string{a.urlOf("q1"), a.urlOf("q2"), a.urlOf("q3"), a.urlOf("q4"), a.urlOf("q5")}, got)
	assert.Equal(t, 3, pages)

	t.Run("a token is bound to its prefix", func(t *testing.T) {
		first, err := a.client().ListQueues(ctx, &sqs.ListQueuesInput{MaxResults: aws.Int32(1)})
		require.NoError(t, err)
		require.NotNil(t, first.NextToken)
		got := readWireError(t, a.raw(t, listTarget, []byte(`{"NextToken":"`+*first.NextToken+`","QueueNamePrefix":"q"}`)))
		assert.Equal(t, 400, got.status)
		assert.Equal(t, "com.amazon.coral.service#InvalidParameterValueException", got.typ)
	})
}

// A token minted for one account cannot page another account's names.
func TestSQSListQueues_TokenIsBoundToTheAccount(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()
	for _, n := range []string{"a1", "a2"} {
		_, err := a.store.Create(ctx, queue.CreateRequest{Account: otherOwner, Region: testRegion, Name: n})
		require.NoError(t, err)
	}
	maxResults := 1
	foreign, err := a.store.List(ctx, queue.ListRequest{Account: otherOwner, Region: testRegion, MaxResults: &maxResults})
	require.NoError(t, err)
	require.NotEmpty(t, foreign.NextToken)

	got := readWireError(t, a.raw(t, listTarget, []byte(`{"NextToken":"`+foreign.NextToken+`"}`)))
	assert.Equal(t, 400, got.status)
	assert.Equal(t, "com.amazon.coral.service#InvalidParameterValueException", got.typ)
}

func TestSQSListQueues_Authorization(t *testing.T) {
	t.Run("a queue-scoped grant does not allow listing", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:ListQueues"}, a0ARN()), true)
		got := readWireError(t, a.raw(t, listTarget, []byte(`{}`)))
		assert.Equal(t, wireError{403, "com.amazon.coral.service#AccessDeniedException", "AccessDenied;Sender", got.message}, got)
	})

	t.Run("an invalid request is rejected before authorizing", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue"}, "*"), true)
		got := readWireError(t, a.raw(t, listTarget, []byte(`{"MaxResults":0}`)))
		assert.Equal(t, "com.amazon.coral.service#InvalidParameterValueException", got.typ)
	})

	t.Run("sqs:ListQueues on * allows", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:ListQueues", "sqs:CreateQueue"}, "*"), true)
		a.create(t, "orders", nil)
		out, err := a.client().ListQueues(context.Background(), &sqs.ListQueuesInput{})
		require.NoError(t, err)
		assert.Equal(t, []string{a.urlOf("orders")}, out.QueueUrls)
	})
}

func a0ARN() string { return "arn:aws:sqs:" + testRegion + ":" + ecrTestAccount + ":orders" }
