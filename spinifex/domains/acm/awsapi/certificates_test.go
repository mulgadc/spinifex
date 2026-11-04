package awsapi

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Invalid JSON bodies are rejected before any NATS round-trip.
func TestOps_InvalidBodyRejected(t *testing.T) {
	bad := []byte("{not json")
	_, err := ImportCertificate(context.Background(), nil, "acct", bad)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidParameter)
	_, err = DescribeCertificate(context.Background(), nil, "acct", bad)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidParameter)
	_, err = ListCertificates(context.Background(), nil, "acct", bad)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidParameter)
	_, err = DeleteCertificate(context.Background(), nil, "acct", bad)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidParameter)
	_, err = ListTagsForCertificate(context.Background(), nil, "acct", bad)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidParameter)
	_, err = AddTagsToCertificate(context.Background(), nil, "acct", bad)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidParameter)
	_, err = RemoveTagsFromCertificate(context.Background(), nil, "acct", bad)
	assert.ErrorContains(t, err, awserrors.ErrorInvalidParameter)
}

// With a live conn but no daemon subscriber, the delegate path fast-fails with
// no-responders — exercising the NATSRequest call site in each op.
func TestOps_DelegateNoResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	_, err := ImportCertificate(context.Background(), nc, "acct", []byte(`{}`))
	require.Error(t, err)
	_, err = DescribeCertificate(context.Background(), nc, "acct", []byte(`{}`))
	require.Error(t, err)
	_, err = ListCertificates(context.Background(), nc, "acct", []byte(`{}`))
	require.Error(t, err)
	_, err = DeleteCertificate(context.Background(), nc, "acct", []byte(`{}`))
	require.Error(t, err)
	_, err = ListTagsForCertificate(context.Background(), nc, "acct", []byte(`{}`))
	require.Error(t, err)
	_, err = AddTagsToCertificate(context.Background(), nc, "acct", []byte(`{}`))
	require.Error(t, err)
	_, err = RemoveTagsFromCertificate(context.Background(), nc, "acct", []byte(`{}`))
	require.Error(t, err)
}
