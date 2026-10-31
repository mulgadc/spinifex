package gateway

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSQS_LambdaPollerPath drives the calls an event source mapping makes, in
// order, through the signed gateway and the real queue and message owners.
func TestSQS_LambdaPollerPath(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	c := a.client()
	ctx := context.Background()

	created, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("lambda-src"),
		Attributes: map[string]string{"VisibilityTimeout": "45"}})
	require.NoError(t, err)
	got, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("lambda-src")})
	require.NoError(t, err)
	u := aws.ToString(got.QueueUrl)
	require.Equal(t, aws.ToString(created.QueueUrl), u)
	rec, err := a.store.Resolve(ctx, ecrTestAccount, testRegion, "lambda-src")
	require.NoError(t, err)

	attrs, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(u),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn, sqstypes.QueueAttributeNameVisibilityTimeout}})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"QueueArn": rec.ARN, "VisibilityTimeout": "45"}, attrs.Attributes)

	entries := make([]sqstypes.SendMessageBatchRequestEntry, 3)
	for i := range entries {
		entries[i] = sqstypes.SendMessageBatchRequestEntry{Id: aws.String("e" + strconv.Itoa(i)), MessageBody: aws.String("job-" + strconv.Itoa(i)),
			MessageAttributes: map[string]sqstypes.MessageAttributeValue{"kind": {DataType: aws.String("String"), StringValue: aws.String("job")}}}
	}
	sent, err := c.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{QueueUrl: aws.String(u), Entries: entries})
	require.NoError(t, err)
	require.Len(t, sent.Successful, 3)
	require.Empty(t, sent.Failed)

	recv, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), MaxNumberOfMessages: 10,
		WaitTimeSeconds: 20, MessageAttributeNames: []string{"All"},
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{sqstypes.MessageSystemAttributeNameAll}})
	require.NoError(t, err)
	require.Len(t, recv.Messages, 3)
	bodies := map[string]bool{}
	for _, m := range recv.Messages {
		bodies[aws.ToString(m.Body)] = true
		assert.Equal(t, "job", aws.ToString(m.MessageAttributes["kind"].StringValue))
		assert.NotEmpty(t, m.Attributes["SenderId"])
		assert.Equal(t, "1", m.Attributes["ApproximateReceiveCount"])
		assert.NotEmpty(t, m.Attributes["ApproximateFirstReceiveTimestamp"])
	}
	assert.Equal(t, map[string]bool{"job-0": true, "job-1": true, "job-2": true}, bodies)

	// A failed record goes back to the queue; the rest are deleted in a batch.
	failed := recv.Messages[0]
	vis, err := c.ChangeMessageVisibilityBatch(ctx, &sqs.ChangeMessageVisibilityBatchInput{QueueUrl: aws.String(u),
		Entries: []sqstypes.ChangeMessageVisibilityBatchRequestEntry{{Id: aws.String("f"), ReceiptHandle: failed.ReceiptHandle, VisibilityTimeout: 0}}})
	require.NoError(t, err)
	require.Len(t, vis.Successful, 1)
	del := make([]sqstypes.DeleteMessageBatchRequestEntry, 0, 2)
	for i, m := range recv.Messages[1:] {
		del = append(del, sqstypes.DeleteMessageBatchRequestEntry{Id: aws.String("d" + strconv.Itoa(i)), ReceiptHandle: m.ReceiptHandle})
	}
	dr, err := c.DeleteMessageBatch(ctx, &sqs.DeleteMessageBatchInput{QueueUrl: aws.String(u), Entries: del})
	require.NoError(t, err)
	require.Len(t, dr.Successful, 2)
	require.Empty(t, dr.Failed)

	again, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), MaxNumberOfMessages: 10, WaitTimeSeconds: 1})
	require.NoError(t, err)
	require.Len(t, again.Messages, 1, "only the released record is redelivered")
	assert.Equal(t, aws.ToString(failed.Body), aws.ToString(again.Messages[0].Body))
	_, err = c.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(u), ReceiptHandle: again.Messages[0].ReceiptHandle})
	require.NoError(t, err)

	start := time.Now()
	empty, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), WaitTimeSeconds: 2})
	require.NoError(t, err)
	assert.Empty(t, empty.Messages)
	assert.GreaterOrEqual(t, time.Since(start), 2*time.Second, "an empty long poll waits its full time")
}
