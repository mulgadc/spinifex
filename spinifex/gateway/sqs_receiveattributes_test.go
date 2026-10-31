package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	unreqString = "zq-unreq-other-é"
	unreqBinary = "zq-unreq-bin\x00"
)

// Digests of recvAttrs subsets come from a separate implementation of the
// developer guide's algorithm over the returned attributes only; whether AWS
// digests the returned or all stored attributes is unestablished.
const (
	recvAllMD5        = "eb68614d950bf9942277ddb1a084db52"
	recvAppMD5        = "2fcf0871ee008a8336fb561ab7ae90e1"
	recvAppleMD5      = "41b4c9f3cbfcc9fb1276a6f519087cf6"
	recvOtherAppIDMD5 = "dbc2e3696d88b5a9704f893ecdcc66f5"
)

func recvAttrs() map[string]sqstypes.MessageAttributeValue {
	return map[string]sqstypes.MessageAttributeValue{
		"app.id":  {DataType: aws.String("String"), StringValue: aws.String("7")},
		"app.tag": {DataType: aws.String("Binary.raw"), BinaryValue: []byte(unreqBinary)},
		"apple":   {DataType: aws.String("String"), StringValue: aws.String("red")},
		"Other":   {DataType: aws.String("String.x"), StringValue: aws.String(unreqString)},
	}
}

func (a sqsRecvApp) sendWithAttrs(t *testing.T, u string) {
	t.Helper()
	_, err := a.client().SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: aws.String(u), MessageBody: aws.String("with attributes"), MessageAttributes: recvAttrs(),
	})
	require.NoError(t, err)
}

func attrNames(m map[string]sqstypes.MessageAttributeValue) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func TestSQSReceiveMessage_MessageAttributeSelectors(t *testing.T) {
	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", map[string]string{"VisibilityTimeout": "0"})
	a.sendWithAttrs(t, u)

	all := []string{"Other", "app.id", "app.tag", "apple"}
	for name, tc := range map[string]struct {
		sel   []string
		names []string
		md5   string
	}{
		"none asked":     {nil, nil, ""},
		"All":            {[]string{"All"}, all, recvAllMD5},
		"dot star":       {[]string{".*"}, all, recvAllMD5},
		"All and a name": {[]string{"All", "apple"}, all, recvAllMD5},
		"dotted prefix":  {[]string{"app.*"}, []string{"app.id", "app.tag"}, recvAppMD5},
		"exact name":     {[]string{"apple"}, []string{"apple"}, recvAppleMD5},
		"two names":      {[]string{"Other", "app.id"}, []string{"Other", "app.id"}, recvOtherAppIDMD5},
		"case sensitive": {[]string{"other", "APPLE"}, nil, ""},
		"absent name":    {[]string{"absent"}, nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := a.receive(t, &sqs.ReceiveMessageInput{
				QueueUrl: aws.String(u), MessageAttributeNames: tc.sel,
			})
			require.Len(t, got, 1)
			if tc.names == nil {
				assert.Empty(t, got[0].MessageAttributes)
				assert.Nil(t, got[0].MD5OfMessageAttributes, "no attributes returned, no digest")
				return
			}
			assert.Equal(t, tc.names, attrNames(got[0].MessageAttributes))
			assert.Equal(t, tc.md5, aws.ToString(got[0].MD5OfMessageAttributes))
			want := recvAttrs()
			for _, n := range tc.names {
				assert.Equal(t, want[n], got[0].MessageAttributes[n], n)
			}
		})
	}
}

// Unrequested values never reach the response or a log line.
func TestSQSReceiveMessage_UnrequestedAttributesNotReturnedOrLogged(t *testing.T) {
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := newSQSRecvApp(t, allowAllECRDocs(), nil)
	u := a.create(t, "orders", map[string]string{"VisibilityTimeout": "0"})
	a.sendWithAttrs(t, u)

	resp := a.raw(t, receiveTarget, recvBody(u, `,"MessageAttributeNames":["apple"]`))
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, string(raw), `"apple"`)
	for _, v := range []string{"zq-unreq", "enEtdW5yZXEtYmluAA==", `"Other"`, `"app.tag"`} {
		assert.NotContains(t, string(raw), v)
	}

	readWireError(t, a.raw(t, receiveTarget, recvBody(u, `,"MessageAttributeNames":["AWS.x"]`)))
	readWireError(t, a.raw(t, receiveTarget, recvBody(u, `,"WaitTimeSeconds":21,"MessageAttributeNames":["All"]`)))
	all := a.receive(t, &sqs.ReceiveMessageInput{QueueUrl: aws.String(u), MessageAttributeNames: []string{"All"}})
	require.Len(t, all, 1)
	require.Positive(t, buf.Len(), "the failure paths must have logged something to inspect")
	assert.NotContains(t, buf.String(), "zq-unreq")
	assert.NotContains(t, buf.String(), "enEtdW5yZXEtYmluAA==")
}
