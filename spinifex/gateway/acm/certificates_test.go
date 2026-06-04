package gateway_acm

import (
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnmarshalIfBody(t *testing.T) {
	var out struct {
		A string `json:"a"`
	}
	require.NoError(t, unmarshalIfBody(nil, &out))
	require.NoError(t, unmarshalIfBody([]byte(`{"a":"x"}`), &out))
	assert.Equal(t, "x", out.A)
	require.Error(t, unmarshalIfBody([]byte("{bad"), &out))
}

func TestWriteJSONResponse(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSONResponse(w, map[string]string{"CertificateArn": "arn:aws:acm:x"})
	assert.Equal(t, JSONContentType, w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), "CertificateArn")
}

// Invalid JSON bodies are rejected before any NATS round-trip.
func TestOps_InvalidBodyRejected(t *testing.T) {
	bad := []byte("{not json")
	_, err := ImportCertificate(nil, "acct", bad)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidParameter)
	_, err = DescribeCertificate(nil, "acct", bad)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidParameter)
	_, err = ListCertificates(nil, "acct", bad)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidParameter)
	_, err = DeleteCertificate(nil, "acct", bad)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidParameter)
}

// With a live conn but no daemon subscriber, the delegate path fast-fails with
// no-responders — exercising the NATSRequest call site in each op.
func TestOps_DelegateNoResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	_, err := ImportCertificate(nc, "acct", []byte(`{}`))
	require.Error(t, err)
	_, err = DescribeCertificate(nc, "acct", []byte(`{}`))
	require.Error(t, err)
	_, err = ListCertificates(nc, "acct", []byte(`{}`))
	require.Error(t, err)
	_, err = DeleteCertificate(nc, "acct", []byte(`{}`))
	require.Error(t, err)
}
