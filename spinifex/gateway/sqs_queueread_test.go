package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	urlTarget   = "AmazonSQS.GetQueueUrl"
	attrsTarget = "AmazonSQS.GetQueueAttributes"
	otherOwner  = "999988887777"
)

func (a sqsApp) create(t *testing.T, name string, attrs map[string]string) string {
	t.Helper()
	out, err := a.client().CreateQueue(context.Background(), &sqs.CreateQueueInput{QueueName: aws.String(name), Attributes: attrs})
	require.NoError(t, err)
	return aws.ToString(out.QueueUrl)
}

func notExist(got wireError) wireError {
	return wireError{400, "com.amazonaws.sqs#QueueDoesNotExist", "AWS.SimpleQueueService.NonExistentQueue;Sender", got.message}
}

func TestSQSGetQueueUrl_SDK(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()
	created := a.create(t, "orders", nil)

	out, err := a.client().GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("orders")})
	require.NoError(t, err)
	assert.Equal(t, created, aws.ToString(out.QueueUrl))

	out, err = a.client().GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("orders"), QueueOwnerAWSAccountId: aws.String(ecrTestAccount)})
	require.NoError(t, err, "naming the caller's own account is allowed")
	assert.Equal(t, created, aws.ToString(out.QueueUrl))

	_, err = a.client().GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("missing")})
	var gone *sqstypes.QueueDoesNotExist
	require.ErrorAs(t, err, &gone)
}

func TestSQSGetQueueUrl_Errors(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	a.create(t, "orders", nil)
	ctx := context.Background()
	a.create(t, "gone", nil)
	_, err := a.store.MarkDeleting(ctx, ecrTestAccount, testRegion, "gone")
	require.NoError(t, err)

	t.Run("missing queue", func(t *testing.T) {
		got := readWireError(t, a.raw(t, urlTarget, []byte(`{"QueueName":"missing"}`)))
		assert.Equal(t, notExist(got), got)
	})
	t.Run("deleting queue", func(t *testing.T) {
		got := readWireError(t, a.raw(t, urlTarget, []byte(`{"QueueName":"gone"}`)))
		assert.Equal(t, notExist(got), got)
	})
	t.Run("another owner is refused", func(t *testing.T) {
		got := readWireError(t, a.raw(t, urlTarget, []byte(`{"QueueName":"orders","QueueOwnerAWSAccountId":"`+otherOwner+`"}`)))
		assert.Equal(t, wireError{404, "com.amazonaws.sqs#InvalidAddress", "InvalidAddress;Sender", "The address " + a.url + "/ is not valid for this endpoint."}, got)
	})
	for name, body := range map[string]string{
		"absent name": `{}`,
		"empty name":  `{"QueueName":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := readWireError(t, a.raw(t, urlTarget, []byte(body)))
			assert.Equal(t, wireError{400, "com.amazon.coral.service#MissingRequiredParameterException", "MissingParameter;Sender", "The request must contain the parameter QueueName."}, got)
		})
	}
	t.Run("invalid name", func(t *testing.T) {
		got := readWireError(t, a.raw(t, urlTarget, []byte(`{"QueueName":"no spaces"}`)))
		assert.Equal(t, 400, got.status)
		assert.Equal(t, "com.amazon.coral.service#InvalidParameterValueException", got.typ)
	})
	t.Run("malformed body", func(t *testing.T) {
		got := readWireError(t, a.raw(t, urlTarget, []byte(`{"QueueName":"orders"}}`)))
		assert.Equal(t, "com.amazon.coral.service#SerializationException", got.typ)
	})
}

func TestSQSGetQueueAttributes_SDK(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()
	u := a.create(t, "orders", map[string]string{"VisibilityTimeout": "45"})
	rec, err := a.store.Resolve(ctx, ecrTestAccount, testRegion, "orders")
	require.NoError(t, err)

	out, err := a.client().GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(u),
		AttributeNames: []sqstypes.QueueAttributeName{
			"VisibilityTimeout", "MaximumMessageSize", "QueueArn", "CreatedTimestamp", "Policy",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "45", out.Attributes["VisibilityTimeout"])
	assert.Equal(t, "1048576", out.Attributes["MaximumMessageSize"])
	assert.Equal(t, arn.FormatSQSQueue(testRegion, ecrTestAccount, "orders"), out.Attributes["QueueArn"])
	assert.Equal(t, rec.ARN, out.Attributes["QueueArn"])
	assert.NotEmpty(t, out.Attributes["CreatedTimestamp"])
	assert.NotContains(t, out.Attributes, "Policy", "an unset policy is absent, as in AWS")

	for _, name := range []sqstypes.QueueAttributeName{
		sqstypes.QueueAttributeNameAll, "ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible", "ApproximateNumberOfMessagesDelayed",
	} {
		_, err = a.client().GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: aws.String(u), AttributeNames: []sqstypes.QueueAttributeName{"VisibilityTimeout", name},
		})
		var unsupported *sqstypes.UnsupportedOperation
		require.ErrorAs(t, err, &unsupported, "%s needs message counts, which are not served yet", name)
	}

	out, err = a.client().GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(u)})
	require.NoError(t, err)
	assert.Empty(t, out.Attributes, "no names asks for nothing")

	_, err = a.client().GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(u), AttributeNames: []sqstypes.QueueAttributeName{"Bogus"},
	})
	var badName *sqstypes.InvalidAttributeName
	require.ErrorAs(t, err, &badName)
}

func TestSQSGetQueueAttributes_Errors(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()
	u := a.create(t, "orders", nil)
	a.create(t, "gone", nil)
	_, err := a.store.MarkDeleting(ctx, ecrTestAccount, testRegion, "gone")
	require.NoError(t, err)
	invalidAddress := wireError{404, "com.amazonaws.sqs#InvalidAddress", "InvalidAddress;Sender", "The address " + a.url + "/ is not valid for this endpoint."}

	for name, tc := range map[string]struct {
		body string
		want wireError
	}{
		"missing queue":               {`{"QueueUrl":"` + a.url + `/` + ecrTestAccount + `/missing"}`, wireError{}},
		"deleting queue":              {`{"QueueUrl":"` + a.url + `/` + ecrTestAccount + `/gone"}`, wireError{}},
		"invalid name":                {`{"QueueUrl":"` + a.url + `/` + ecrTestAccount + `/no%20spaces"}`, wireError{}},
		"another account's queue URL": {`{"QueueUrl":"` + a.url + `/` + otherOwner + `/orders"}`, invalidAddress},
		"not a URL":                   {`{"QueueUrl":"orders"}`, invalidAddress},
		"no account segment":          {`{"QueueUrl":"` + a.url + `/orders"}`, invalidAddress},
		"extra path segment":          {`{"QueueUrl":"` + u + `/x"}`, invalidAddress},
		"query string":                {`{"QueueUrl":"` + u + `?a=b"}`, invalidAddress},
		"non-HTTP scheme":             {`{"QueueUrl":"ftp://host/` + ecrTestAccount + `/orders"}`, invalidAddress},
		"another host":                {`{"QueueUrl":"` + strings.Replace(u, "127.0.0.1", "localhost", 1) + `"}`, invalidAddress},
		"another port":                {`{"QueueUrl":"` + a.url + `1/` + ecrTestAccount + `/orders"}`, invalidAddress},
		"another Region's AWS host":   {`{"QueueUrl":"https://sqs.us-west-2.amazonaws.com/` + ecrTestAccount + `/orders"}`, invalidAddress},
		"other scheme":                {`{"QueueUrl":"` + strings.Replace(u, "http://", "https://", 1) + `"}`, invalidAddress},
		"user info":                   {`{"QueueUrl":"` + strings.Replace(u, "http://", "http://x@", 1) + `"}`, invalidAddress},
		"empty query":                 {`{"QueueUrl":"` + u + `?"}`, invalidAddress},
		"absent URL": {`{}`, wireError{400, "com.amazon.coral.service#MissingRequiredParameterException", "MissingParameter;Sender",
			"The request must contain the parameter QueueUrl."}},
		"unknown attribute name": {`{"QueueUrl":"` + u + `","AttributeNames":["Bogus"]}`,
			wireError{400, "com.amazonaws.sqs#InvalidAttributeName", "InvalidAttributeName;Sender", ""}},
	} {
		t.Run(name, func(t *testing.T) {
			got := readWireError(t, a.raw(t, attrsTarget, []byte(tc.body)))
			want := tc.want
			if want == (wireError{}) {
				want = notExist(got)
			}
			if want.message == "" {
				want.message = got.message
			}
			assert.Equal(t, want, got)
		})
	}
}

func TestSQSGetQueueAttributes_AuthorizesOnTheQueueARN(t *testing.T) {
	ordersARN := arn.FormatSQSQueue(testRegion, ecrTestAccount, "orders")

	t.Run("grant on another queue is denied before the lookup", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:GetQueueAttributes"}, arn.FormatSQSQueue(testRegion, ecrTestAccount, "other")), true)
		for _, name := range []string{"orders", "missing"} {
			body := `{"QueueUrl":"` + a.url + `/` + ecrTestAccount + `/` + name + `","AttributeNames":["QueueArn"]}`
			got := readWireError(t, a.raw(t, attrsTarget, []byte(body)))
			assert.Equal(t, 403, got.status, name)
			assert.Equal(t, "com.amazon.coral.service#AccessDeniedException", got.typ, name)
		}
	})

	t.Run("an invalid attribute name is rejected before authorizing", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:GetQueueAttributes"}, arn.FormatSQSQueue(testRegion, ecrTestAccount, "other")), true)
		body := `{"QueueUrl":"` + a.url + `/` + ecrTestAccount + `/orders","AttributeNames":["Bogus"]}`
		got := readWireError(t, a.raw(t, attrsTarget, []byte(body)))
		assert.Equal(t, "com.amazonaws.sqs#InvalidAttributeName", got.typ)
	})

	t.Run("grant on the queue ARN allows", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue", "sqs:GetQueueAttributes"}, ordersARN), true)
		u := a.create(t, "orders", nil)
		out, err := a.client().GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{
			QueueUrl: aws.String(u), AttributeNames: []sqstypes.QueueAttributeName{"QueueArn"},
		})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"QueueArn": ordersARN}, out.Attributes)
	})
}
