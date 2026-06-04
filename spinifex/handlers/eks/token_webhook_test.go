package handlers_eks

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeGetToken_RoundTrips(t *testing.T) {
	url := "https://sts.amazonaws.com/?Action=GetCallerIdentity&Version=2011-06-15&X-Amz-Algorithm=AWS4-HMAC-SHA256"
	token := getTokenV1Prefix + base64.RawURLEncoding.EncodeToString([]byte(url))

	got, err := DecodeGetToken(token)
	require.NoError(t, err)
	assert.Equal(t, url, got)
}

func TestDecodeGetToken_TrimsWhitespace(t *testing.T) {
	url := "https://sts.amazonaws.com/?Action=GetCallerIdentity"
	token := "  " + getTokenV1Prefix + base64.RawURLEncoding.EncodeToString([]byte(url)) + "\n"

	got, err := DecodeGetToken(token)
	require.NoError(t, err)
	assert.Equal(t, url, got)
}

func TestDecodeGetToken_RejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"missing prefix": base64.RawURLEncoding.EncodeToString([]byte("https://sts")),
		"empty":          "",
		"prefix only":    getTokenV1Prefix,
		"bad base64":     getTokenV1Prefix + "!!!not-base64!!!",
		"padded base64":  getTokenV1Prefix + base64.URLEncoding.EncodeToString([]byte("https://sts.amazonaws.com/?x=12")),
		"wrong scheme":   "k8s-aws-v2." + base64.RawURLEncoding.EncodeToString([]byte("https://sts")),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeGetToken(tok)
			assert.ErrorIs(t, err, ErrMalformedToken)
		})
	}
}
