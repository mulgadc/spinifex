package message_test

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func str(name, dataType, v string) message.Attribute {
	return message.Attribute{Name: name, Value: message.AttributeValue{DataType: dataType, StringValue: &v}}
}

func bin(name, dataType string, v []byte) message.Attribute {
	return message.Attribute{Name: name, Value: message.AttributeValue{DataType: dataType, BinaryValue: v}}
}

func mustAttrs(t *testing.T, in ...message.Attribute) message.Attributes {
	t.Helper()
	a, err := message.NewAttributes(in)
	require.NoError(t, err)
	return a
}

// The expected digests come from a separate implementation of the
// developer guide's algorithm, not from AWS; they are provisional.
func TestAttributesMD5_DocumentedAlgorithmVectors(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []message.Attribute
		want string
	}{
		"single string": {[]message.Attribute{str("a", "String", "x")}, "a512aac9938034acb0b3c3d965944bf1"},
		"three types": {[]message.Attribute{
			str("Name", "String", "Alice"), str("Count", "Number", "42"), bin("Blob", "Binary", []byte{0, 1, 2, 255}),
		}, "655be3ff51c2255322ad67ede1a57544"},
		"custom suffix":   {[]message.Attribute{str("n", "Number.int", "7")}, "f3c2a2801c523d26c8996134de718052"},
		"no suffix":       {[]message.Attribute{str("n", "Number", "7")}, "3a734e6fc44468814b287b503a526bff"},
		"utf-8 value":     {[]message.Attribute{str("greet", "String", "héllo 世界 😀")}, "d3d5cb26f19f5ac251fb595d2aad84eb"},
		"byte-wise order": {[]message.Attribute{str("b", "String", "1"), str("B", "String", "2"), str("a", "String", "3")}, "cd783096648921dba095ec2755c7ef34"},
		"guide example": {[]message.Attribute{
			str("Author", "String", "John Grisham"), str("Title", "String", "The Whistler"), str("WeeksOn", "Number", "6"),
		}, "d25a6aea97eb8f585bfa92d314504a92"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, mustAttrs(t, tc.in...).MD5())
		})
	}
}

func TestAttributesMD5_IndependentOfSuppliedOrder(t *testing.T) {
	a := mustAttrs(t, str("z", "String", "1"), str("m", "Number", "2"), bin("a", "Binary", []byte{9}))
	b := mustAttrs(t, bin("a", "Binary", []byte{9}), str("z", "String", "1"), str("m", "Number", "2"))
	assert.Equal(t, a.MD5(), b.MD5())
	names := []string{}
	for _, at := range a.All() {
		names = append(names, at.Name)
	}
	assert.Equal(t, []string{"a", "m", "z"}, names)
}

func TestAttributes_EncodeRoundTrip(t *testing.T) {
	raw := []byte{0, 0xff, 0x80, 'x'}
	a := mustAttrs(t, str("s", "String.custom label", "héllo"), str("n", "Number.float", "-1.5e3"), bin("b", "Binary.gz", raw))
	enc := a.Encode()
	sum := md5.Sum(enc[1:])
	assert.Equal(t, a.MD5(), hex.EncodeToString(sum[:]), "the encoding carries the digested bytes")

	got, err := message.DecodeAttributes(enc)
	require.NoError(t, err)
	assert.Equal(t, a.All(), got.All())
	assert.Equal(t, a.MD5(), got.MD5())
	for _, at := range got.All() {
		if at.Name == "b" {
			assert.Nil(t, at.Value.StringValue, "binary stays binary")
			assert.Equal(t, raw, at.Value.BinaryValue)
		} else {
			assert.Nil(t, at.Value.BinaryValue, "string stays string")
		}
	}
}

func TestNewAttributes_Names(t *testing.T) {
	for _, ok := range []string{"a", "A-b_c.d", strings.Repeat("n", 256), "awsx", "Amazonian", "x.AWS.y", "0"} {
		_, err := message.NewAttributes([]message.Attribute{str(ok, "String", "v")})
		assert.NoError(t, err, ok)
	}
	for _, bad := range []string{
		"", strings.Repeat("n", 257), "has space", "é", "a/b", "AWS.x", "aws.x", "aWs.x", "Amazon.x", "AMAZON.x",
		".a", "a.", "a..b", "a\x00",
	} {
		_, err := message.NewAttributes([]message.Attribute{str(bad, "String", "v")})
		assert.ErrorIs(t, err, message.ErrInvalidParameter, "%q", bad)
	}
}

func TestNewAttributes_DataTypes(t *testing.T) {
	for _, ok := range []string{"String", "Number", "Binary", "String.x", "Number.int", "Binary.png", "String.a.b", "String.label with space", "String." + strings.Repeat("é", 249)} {
		v := message.AttributeValue{DataType: ok}
		if strings.HasPrefix(ok, "Binary") {
			v.BinaryValue = []byte{1}
		} else {
			s := "1"
			v.StringValue = &s
		}
		_, err := message.NewAttributes([]message.Attribute{{Name: "a", Value: v}})
		assert.NoError(t, err, ok)
	}
	for _, bad := range []string{"", "string", "STRING", "Strings", "Text", "String.", ".String", "String" + "\x01", "String." + strings.Repeat("x", 250)} {
		_, err := message.NewAttributes([]message.Attribute{str("a", bad, "1")})
		assert.ErrorIs(t, err, message.ErrInvalidParameter, "%q", bad)
	}
}

func TestNewAttributes_ValuePresence(t *testing.T) {
	empty, one := "", "1"
	for name, v := range map[string]message.AttributeValue{
		"string missing":             {DataType: "String"},
		"string empty":               {DataType: "String", StringValue: &empty},
		"number missing":             {DataType: "Number"},
		"binary missing":             {DataType: "Binary"},
		"binary empty":               {DataType: "Binary", BinaryValue: []byte{}},
		"string with binary":         {DataType: "String", StringValue: &one, BinaryValue: []byte{1}},
		"binary with string":         {DataType: "Binary", StringValue: &one, BinaryValue: []byte{1}},
		"binary given only a string": {DataType: "Binary", StringValue: &one},
		"string given only binary":   {DataType: "String", BinaryValue: []byte{1}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := message.NewAttributes([]message.Attribute{{Name: "a", Value: v}})
			assert.ErrorIs(t, err, message.ErrInvalidParameter)
		})
	}
	// Missing and empty are told apart.
	_, missingErr := message.NewAttributes([]message.Attribute{{Name: "a", Value: message.AttributeValue{DataType: "String"}}})
	_, emptyErr := message.NewAttributes([]message.Attribute{{Name: "a", Value: message.AttributeValue{DataType: "String", StringValue: &empty}}})
	assert.Contains(t, missingErr.Error(), "missing")
	assert.Contains(t, emptyErr.Error(), "empty")
}

func TestNewAttributes_StringValueCharacters(t *testing.T) {
	for _, ok := range []string{"tab\there", "line\nfeed", "\r", "�", "😀", ""} {
		_, err := message.NewAttributes([]message.Attribute{str("a", "String", ok)})
		assert.NoError(t, err, "%q", ok)
	}
	for _, bad := range []string{"\x00", "\x1f", "￾", "\xff", "a\xc3"} {
		_, err := message.NewAttributes([]message.Attribute{str("a", "String", bad)})
		require.ErrorIs(t, err, message.ErrInvalidParameter, "%q", bad)
		assert.NotContains(t, err.Error(), bad)
	}
	// A binary value carries any bytes.
	_, err := message.NewAttributes([]message.Attribute{bin("a", "Binary", []byte{0, 0xff, 0xfe})})
	assert.NoError(t, err)
}

func TestNewAttributes_Numbers(t *testing.T) {
	for _, ok := range []string{
		"0", "-0", "+1", "42", "-3.25", ".5", "5.", "1e10", "1E-10", "1e+5", "00012", "0.000",
		strings.Repeat("9", 38), "1" + strings.Repeat("0", 50), "0." + strings.Repeat("0", 127) + "1",
		"1e126", "1.000e126", "-1e126", "1e-128", "9.99e125", "100e124", "0e999999",
	} {
		_, err := message.NewAttributes([]message.Attribute{str("n", "Number", ok)})
		assert.NoError(t, err, ok)
	}
	for _, bad := range []string{
		"", "-", "+", ".", "abc", "1a", "1.2.3", "1e", "1e+", "e5", "--1", "1 ", " 1", "0x10", "Infinity", "NaN", "1,000",
		strings.Repeat("9", 39), "1." + strings.Repeat("1", 38), "1.1e126", "2e126", "1e127", "11e125", "9.9e-129", "1e-129",
		"0." + strings.Repeat("0", 128) + "1", "1e9999999",
	} {
		_, err := message.NewAttributes([]message.Attribute{str("n", "Number", bad)})
		assert.ErrorIs(t, err, message.ErrInvalidParameter, "%q", bad)
	}
	// A custom suffix keeps Number validation.
	_, err := message.NewAttributes([]message.Attribute{str("n", "Number.int", "x")})
	assert.ErrorIs(t, err, message.ErrInvalidParameter)
}

func TestNewAttributes_CountAndDuplicates(t *testing.T) {
	var ten []message.Attribute
	for i := range 10 {
		ten = append(ten, str(fmt.Sprintf("a%d", i), "String", "v"))
	}
	a, err := message.NewAttributes(ten)
	require.NoError(t, err)
	assert.Equal(t, 10, a.Len())

	_, err = message.NewAttributes(append(ten, str("a10", "String", "v")))
	assert.ErrorIs(t, err, message.ErrInvalidParameter)

	_, err = message.NewAttributes([]message.Attribute{str("dup", "String", "1"), bin("dup", "Binary", []byte{1})})
	assert.ErrorIs(t, err, message.ErrInvalidParameter)
	// Names are case-sensitive, so these are distinct.
	_, err = message.NewAttributes([]message.Attribute{str("Dup", "String", "1"), str("dup", "String", "1")})
	assert.NoError(t, err)
}

func TestNewAttributes_ErrorsNeverCarryValues(t *testing.T) {
	secret := "s3cr3t-value"
	for _, in := range []message.Attribute{
		str("n", "Number", secret),
		str("s", "String", secret+"\x00"),
		{Name: "b", Value: message.AttributeValue{DataType: "Binary", StringValue: &secret, BinaryValue: []byte(secret)}},
		{Name: "l", Value: message.AttributeValue{DataType: "String", StringValue: &secret, StringListValues: []string{secret}}},
	} {
		_, err := message.NewAttributes([]message.Attribute{in})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), secret)
	}
}

func TestAttributes_SizeAndCombinedLimit(t *testing.T) {
	a := mustAttrs(t, str("ab", "String", "héllo"), bin("c", "Binary.gz", []byte{1, 2, 3}))
	// name + data type + value bytes: (2+6+6) + (1+9+3).
	assert.Equal(t, 27, a.Size())

	const limit = message.MaxMessageSize
	assert.NoError(t, message.CheckMessageSize(limit-27, a, limit))
	assert.ErrorIs(t, message.CheckMessageSize(limit-26, a, limit), message.ErrInvalidParameter)
	assert.NoError(t, message.CheckMessageSize(limit, mustAttrs(t), limit))
	assert.ErrorIs(t, message.CheckMessageSize(1024, a, 1024), message.ErrInvalidParameter)

	big := mustAttrs(t, bin("b", "Binary", make([]byte, limit-len("b")-len("Binary"))))
	assert.Equal(t, limit, big.Size())
	assert.NoError(t, message.CheckMessageSize(0, big, limit))
	assert.ErrorIs(t, message.CheckMessageSize(1, big, limit), message.ErrInvalidParameter)

	_, err := message.NewAttributes([]message.Attribute{bin("b", "Binary", make([]byte, limit-len("b")-len("Binary")+1))})
	assert.ErrorIs(t, err, message.ErrInvalidParameter, "a set no queue could accept is refused outright")
}

func selectedNames(a message.Attributes) []string {
	var out []string
	for _, at := range a.All() {
		out = append(out, at.Name)
	}
	return out
}

func TestAttributeSelector_Selects(t *testing.T) {
	set := mustAttrs(t, str("app.id", "String", "1"), str("app.x.y", "String", "2"), str("apple", "String", "3"), str("b", "String", "4"))
	for name, tc := range map[string]struct {
		in   []string
		want []string
	}{
		"nothing asked":       {nil, nil},
		"All":                 {[]string{"All"}, []string{"app.id", "app.x.y", "apple", "b"}},
		"dot star":            {[]string{".*"}, []string{"app.id", "app.x.y", "apple", "b"}},
		"prefix stops at dot": {[]string{"app.*"}, []string{"app.id", "app.x.y"}},
		"nested prefix":       {[]string{"app.x.*"}, []string{"app.x.y"}},
		"exact":               {[]string{"apple", "b"}, []string{"apple", "b"}},
		"exact is not prefix": {[]string{"app"}, nil},
		"case sensitive":      {[]string{"B", "APP.*"}, nil},
		"repeated selector":   {[]string{"b", "b"}, []string{"b"}},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := message.ParseAttributeSelector(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, selectedNames(set.Select(s)))
		})
	}
}

func TestAttributes_SelectDigestsSubset(t *testing.T) {
	set := mustAttrs(t, bin("a.bin", "Binary", []byte{1, 2}), str("b", "String", "x"))
	s, err := message.ParseAttributeSelector([]string{"a.*"})
	require.NoError(t, err)
	sub := set.Select(s)
	assert.Equal(t, mustAttrs(t, bin("a.bin", "Binary", []byte{1, 2})).MD5(), sub.MD5())
	assert.NotEqual(t, set.MD5(), sub.MD5())
}
