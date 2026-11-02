package jetstream_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"strconv"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strAttr(name, dataType, v string) message.Attribute {
	return message.Attribute{Name: name, Value: message.AttributeValue{DataType: dataType, StringValue: &v}}
}

func binAttr(name, dataType string, v []byte) message.Attribute {
	return message.Attribute{Name: name, Value: message.AttributeValue{DataType: dataType, BinaryValue: v}}
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func TestSendWithAttributesStoresVersionedRecordAndReceivesThem(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", nil)
	in := input("orders", "hello")
	in.MessageAttributes = []message.Attribute{strAttr("Name", "String", "Alice"), binAttr("Blob", "Binary.gz", []byte{0, 1, 2, 255})}
	res, err := e.sender(nil).Send(context.Background(), in)
	require.NoError(t, err)
	// From a separate implementation of the documented algorithm, not AWS.
	assert.Equal(t, "7ba25f6d674965cfffd95c5a417584dc", res.MD5OfMessageAttributes)
	assert.Equal(t, md5Hex([]byte("hello")), res.MD5OfBody)

	raw, err := e.store.Get(context.Background(), q.Namespace(), res.Position)
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(message.MetaVersion), raw.Meta[message.MetaV], "a reader without attribute support refuses this record")
	assert.Equal(t, res.MD5OfMessageAttributes, raw.Meta[message.MetaMD5A])
	assert.NotEqual(t, []byte("hello"), raw.Body, "the stored payload is not a plain body")
	assert.True(t, bytes.HasSuffix(raw.Body, []byte("hello")))

	got, err := e.receiver(nil).Receive(context.Background(), recv("orders", 1))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, []byte("hello"), got[0].Body)
	assert.Equal(t, res.MD5OfBody, got[0].MD5OfBody)
	assert.Equal(t, res.MD5OfMessageAttributes, got[0].MD5OfMessageAttributes)
	want, err := message.NewAttributes(in.MessageAttributes)
	require.NoError(t, err)
	assert.Equal(t, want.All(), got[0].Attributes.All())
}

func TestSendWithoutAttributesKeepsPlainRecord(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", nil)
	res := e.send(t, "orders", "plain", nil)
	assert.Empty(t, res.MD5OfMessageAttributes)

	raw, err := e.store.Get(context.Background(), q.Namespace(), res.Position)
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(message.MetaVersionPlain), raw.Meta[message.MetaV])
	assert.NotContains(t, raw.Meta, message.MetaMD5A)
	assert.Equal(t, []byte("plain"), raw.Body)
}

// A record as an earlier release wrote it, appended directly, still reads.
func TestReceiveReadsEarlierPlainRecords(t *testing.T) {
	e := newReceiveEnv(t)
	q := e.create(t, acct, "orders", nil)
	enq := strconv.FormatInt(fixedNow.UnixMilli(), 10)
	_, err := e.store.Append(context.Background(), q.Namespace(), message.AppendInput{Body: []byte("from before"), Meta: map[string]string{
		message.MetaV: "1", message.MetaID: "11111111-1111-4111-8111-111111111111", message.MetaEnq: enq, message.MetaVis: enq,
		message.MetaSender: sender, message.MetaMD5: md5Hex([]byte("from before")), message.MetaAcct: acct,
	}})
	require.NoError(t, err)

	got, err := e.receiver(nil).Receive(context.Background(), recv("orders", 1))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, []byte("from before"), got[0].Body)
	assert.Zero(t, got[0].Attributes.Len())
	assert.Empty(t, got[0].MD5OfMessageAttributes)
}
