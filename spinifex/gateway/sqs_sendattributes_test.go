package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Expected attribute digests come from a separate implementation of the
// developer guide's algorithm, not from AWS; they are provisional.
const (
	sdkAttrsMD5 = "61982ecd7e06605b50f96ac7b7b9d012"
	pairMD5     = "51a4d2e1498f4e40cd88088b151e1328"
	fffdMD5     = "ceb538ba3c0e5f5853feb4a0a668e864"
	oneAttrMD5  = "0a77b0642718c3d9dd47090567d11e0d"
)

func sdkAttrs() map[string]sqstypes.MessageAttributeValue {
	return map[string]sqstypes.MessageAttributeValue{
		"Author": {DataType: aws.String("String"), StringValue: aws.String("Grisham")},
		"Lang":   {DataType: aws.String("String.iso"), StringValue: aws.String("fr-é")},
		"Blob":   {DataType: aws.String("Binary"), BinaryValue: []byte{0, 255, 1}},
		"Png":    {DataType: aws.String("Binary.png"), BinaryValue: []byte("\x89PNG")},
	}
}

// storedAttrs checks a stored record is the versioned attribute form whose
// payload ends with body, and returns its recorded attribute digest.
func storedAttrs(t *testing.T, m message.Message, body string) string {
	t.Helper()
	assert.Equal(t, strconv.Itoa(message.MetaVersion), m.Meta[message.MetaV])
	assert.NotEqual(t, body, string(m.Body), "the payload is not a plain body")
	assert.True(t, strings.HasSuffix(string(m.Body), body))
	assert.Equal(t, md5Hex([]byte(body)), m.Meta[message.MetaMD5])
	return m.Meta[message.MetaMD5A]
}

func TestSQSSendMessage_SDKAttributesStoredWithDigest(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "orders", nil)

	out, err := a.client().SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl: aws.String(u), MessageBody: aws.String("hello"), MessageAttributes: sdkAttrs(),
	})
	require.NoError(t, err)
	assert.Equal(t, md5Hex([]byte("hello")), aws.ToString(out.MD5OfMessageBody))
	assert.Equal(t, sdkAttrsMD5, aws.ToString(out.MD5OfMessageAttributes))
	assert.Equal(t, sdkAttrsMD5, storedAttrs(t, a.stored(t, q, 0), "hello"))

	plain, err := a.client().SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String("plain")})
	require.NoError(t, err)
	assert.Nil(t, plain.MD5OfMessageAttributes, "no attributes, no digest")
	m := a.stored(t, q, 1)
	assert.Equal(t, strconv.Itoa(message.MetaVersionPlain), m.Meta[message.MetaV])
	assert.Equal(t, []byte("plain"), m.Body)
}

// Raw JSON text is checked before encoding/json could repair it, so a
// malformed name or string is refused while U+FFFD and valid pairs are kept.
func TestSQSSendMessage_AttributeWireText(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "orders", nil)
	attrBody := func(attrs string) []byte { return sendBody(u, `,"MessageAttributes":`+attrs) }

	for name, tc := range map[string]struct {
		attrs, md5 string
	}{
		"escaped pair":   {`{"k":{"DataType":"String","StringValue":"\ud83d\ude00"}}`, pairMD5},
		"literal pair":   {`{"k":{"DataType":"String","StringValue":"😀"}}`, pairMD5},
		"escaped U+FFFD": {`{"k":{"DataType":"String","StringValue":"\ufffd"}}`, fffdMD5},
		"literal U+FFFD": {`{"k":{"DataType":"String","StringValue":"�"}}`, fffdMD5},
		"escaped name":   {`{"\u006b":{"DataType":"String","StringValue":"\u0076"}}`, oneAttrMD5},
		"null members":   {`{"k":{"DataType":"String","StringValue":"v","BinaryValue":null,"StringListValues":null}}`, oneAttrMD5},
	} {
		t.Run(name, func(t *testing.T) {
			resp := a.raw(t, sendTarget, attrBody(tc.attrs))
			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(b))
			var out struct {
				MD5OfMessageAttributes string `json:"MD5OfMessageAttributes"`
			}
			require.NoError(t, json.Unmarshal(b, &out))
			assert.Equal(t, tc.md5, out.MD5OfMessageAttributes)
		})
	}
	sent := a.calls()

	invalid := wireError{400, "com.amazon.coral.service#InvalidParameterValueException", "InvalidParameterValue;Sender", ""}
	unsupported := wireError{400, "com.amazonaws.sqs#UnsupportedOperation", "AWS.SimpleQueueService.UnsupportedOperation;Sender", ""}
	serialization := wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}
	for name, tc := range map[string]struct {
		attrs string
		want  wireError
	}{
		"malformed UTF-8 name":    {"{\"k\xff\":{\"DataType\":\"String\",\"StringValue\":\"v\"}}", invalid},
		"malformed UTF-8 value":   {"{\"k\":{\"DataType\":\"String\",\"StringValue\":\"a\xc3\"}}", invalid},
		"malformed UTF-8 type":    {"{\"k\":{\"DataType\":\"String.\xfe\",\"StringValue\":\"v\"}}", invalid},
		"unpaired high surrogate": {`{"k":{"DataType":"String","StringValue":"\ud83d"}}`, invalid},
		"unpaired low surrogate":  {`{"k":{"DataType":"String","StringValue":"\ude00x"}}`, invalid},
		"surrogate in name":       {`{"k\udc00":{"DataType":"String","StringValue":"v"}}`, invalid},
		"repeated name":           {`{"k":{"DataType":"String","StringValue":"1"},"k":{"DataType":"String","StringValue":"2"}}`, invalid},
		"repeated member":         {`{"k":{"DataType":"String","StringValue":"1","StringValue":"2"}}`, invalid},
		"member wrong case":       {`{"k":{"DataType":"String","stringValue":"1"}}`, invalid},
		"unknown member":          {`{"k":{"DataType":"String","StringValue":"1","Extra":"x"}}`, invalid},
		"reserved name":           {`{"aws.k":{"DataType":"String","StringValue":"v"}}`, invalid},
		"empty string value":      {`{"k":{"DataType":"String","StringValue":""}}`, invalid},
		"missing string value":    {`{"k":{"DataType":"String"}}`, invalid},
		"empty binary value":      {`{"k":{"DataType":"Binary","BinaryValue":""}}`, invalid},
		"both values":             {`{"k":{"DataType":"String","StringValue":"v","BinaryValue":"AQ=="}}`, invalid},
		"lower-case type":         {`{"k":{"DataType":"string","StringValue":"v"}}`, invalid},
		"missing type":            {`{"k":{"StringValue":"v"}}`, invalid},
		"disallowed character":    {`{"k":{"DataType":"String","StringValue":"a\u0000"}}`, invalid},
		"number":                  {`{"k":{"DataType":"Number","StringValue":"1.50"}}`, unsupported},
		"custom number":           {`{"k":{"DataType":"Number.int","StringValue":"1"}}`, unsupported},
		"string list":             {`{"k":{"DataType":"String","StringValue":"v","StringListValues":[]}}`, unsupported},
		"binary list":             {`{"k":{"DataType":"Binary","BinaryValue":"AQ==","BinaryListValues":["AQ=="]}}`, unsupported},
		"bad base64":              {`{"k":{"DataType":"Binary","BinaryValue":"not base64!"}}`, serialization},
		"not an object":           {`[{"k":1}]`, serialization},
		"value not an object":     {`{"k":"v"}`, serialization},
		"type not a string":       {`{"k":{"DataType":1,"StringValue":"v"}}`, serialization},
		"eleven attributes":       {elevenAttrs(), invalid},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, withoutMessage(readWireError(t, a.raw(t, sendTarget, attrBody(tc.attrs)))))
		})
	}
	// A supplied empty value is told apart from a missing one.
	assert.Contains(t, readWireError(t, a.raw(t, sendTarget, attrBody(`{"k":{"DataType":"Binary","BinaryValue":""}}`))).message, "empty BinaryValue")
	assert.Contains(t, readWireError(t, a.raw(t, sendTarget, attrBody(`{"k":{"DataType":"Binary"}}`))).message, "missing its BinaryValue")
	assert.Equal(t, sent, a.calls(), "invalid attributes never reach the message owner")
	assert.Equal(t, uint64(sent), a.count(t, q))
}

func elevenAttrs() string {
	var parts []string
	for i := range 11 {
		parts = append(parts, `"a`+strconv.Itoa(i)+`":{"DataType":"String","StringValue":"v"}`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Errors name an attribute but never echo its value.
func TestSQSSendMessage_AttributeErrorsCarryNoValue(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, _ := a.create(t, "orders", nil)
	const marker = "VALUEMARKER7731"
	for _, attrs := range []string{
		`{"k":{"DataType":"String","StringValue":"` + marker + `\u0000"}}`,
		`{"k":{"DataType":"Number","StringValue":"` + marker + `"}}`,
		`{"k":{"DataType":"String","StringValue":"` + marker + `","BinaryValue":"AQ=="}}`,
	} {
		b, err := io.ReadAll(a.raw(t, sendTarget, sendBody(u, `,"MessageAttributes":`+attrs)).Body)
		require.NoError(t, err)
		assert.NotContains(t, string(b), marker)
	}
}

// A full-sized combined message passes through the gateway, the sender and
// a store at the guest's 2 MiB payload limit.
func TestSQSSendMessage_FullSizeBodyWithAttributes(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "orders", nil)
	attrs := map[string]sqstypes.MessageAttributeValue{
		"blob": {DataType: aws.String("Binary"), BinaryValue: []byte(strings.Repeat("\xff", 300<<10))},
		"text": {DataType: aws.String("String"), StringValue: aws.String(strings.Repeat("é", 1000))},
	}
	body := strings.Repeat("a", message.MaxMessageSize-len("blob")-len("Binary")-300<<10-len("text")-len("String")-2000)
	out, err := a.client().SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String(body), MessageAttributes: attrs})
	require.NoError(t, err)
	assert.NotEmpty(t, aws.ToString(out.MD5OfMessageAttributes))
	assert.Equal(t, aws.ToString(out.MD5OfMessageAttributes), storedAttrs(t, a.stored(t, q, 0), body))

	_, err = a.client().SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(u), MessageBody: aws.String(body + "a"), MessageAttributes: attrs})
	require.Error(t, err)
	assert.Equal(t, uint64(1), a.count(t, q))
}

func (a sqsSendApp) count(t *testing.T, q queue.Record) uint64 {
	t.Helper()
	u, err := a.msgs.Count(context.Background(), q.Namespace())
	require.NoError(t, err)
	return u.Messages
}

func TestSQSSendMessageBatch_AttributesPerEntry(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "orders", nil)

	out, err := a.client().SendMessageBatch(context.Background(), &sqs.SendMessageBatchInput{QueueUrl: aws.String(u), Entries: []sqstypes.SendMessageBatchRequestEntry{
		{Id: aws.String("with"), MessageBody: aws.String("tagged"), MessageAttributes: sdkAttrs()},
		{Id: aws.String("without"), MessageBody: aws.String("plain")},
		{Id: aws.String("reserved"), MessageBody: aws.String("bad"), MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			"Amazon.k": {DataType: aws.String("String"), StringValue: aws.String("v")},
		}},
		{Id: aws.String("number"), MessageBody: aws.String("num"), MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			"n": {DataType: aws.String("Number"), StringValue: aws.String("1")},
		}},
	}})
	require.NoError(t, err)
	require.Len(t, out.Successful, 2)
	assert.Equal(t, "with", aws.ToString(out.Successful[0].Id))
	assert.Equal(t, sdkAttrsMD5, aws.ToString(out.Successful[0].MD5OfMessageAttributes))
	assert.Equal(t, "without", aws.ToString(out.Successful[1].Id))
	assert.Nil(t, out.Successful[1].MD5OfMessageAttributes)
	got := map[string]string{}
	for _, f := range out.Failed {
		assert.True(t, f.SenderFault)
		got[aws.ToString(f.Id)] = aws.ToString(f.Code)
	}
	assert.Equal(t, map[string]string{"reserved": "InvalidParameterValue", "number": "AWS.SimpleQueueService.UnsupportedOperation"}, got)

	stored := map[string]message.Message{}
	for pos := message.Position(1); pos <= 12; pos++ {
		if m, err := a.msgs.Get(context.Background(), q.Namespace(), pos); err == nil {
			stored[m.Meta[message.MetaID]] = m
		}
	}
	require.Len(t, stored, 2, "refused entries are never stored")
	assert.Equal(t, sdkAttrsMD5, storedAttrs(t, stored[aws.ToString(out.Successful[0].MessageId)], "tagged"))
}

// Attribute bytes count toward the batch total with the bodies.
func TestSQSSendMessageBatch_AttributesCountTowardBatchTotal(t *testing.T) {
	a := newSQSSendApp(t, allowAllECRDocs(), nil)
	u, q := a.create(t, "orders", nil)
	half := message.MaxMessageSize / 2
	attr := `,"MessageAttributes":{"k":{"DataType":"String","StringValue":"vvvv"}}` // 1 + 6 + 4 bytes
	entries := func(extra int) []byte {
		return sendBatchBody(t, u, sendEntry(t, "a", strings.Repeat("a", half-11), attr), sendEntry(t, "b", strings.Repeat("b", half+extra), ""))
	}
	assert.Equal(t, "AWS.SimpleQueueService.BatchRequestTooLong;Sender", readWireError(t, a.raw(t, sendBatchTarget, entries(1))).queryError)
	assert.Equal(t, uint64(0), a.count(t, q))

	out, _ := readSendBatch(t, a.raw(t, sendBatchTarget, entries(0)))
	assert.Len(t, out.Successful, 2, "exactly the batch limit is accepted")
}
