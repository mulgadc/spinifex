package handlers_acm

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/require"
)

// With no daemon subscribed to acm.*, each NATS request fast-fails with
// no-responders. The point is to exercise the client delegation paths.
func TestNATSACMService_NoResponder(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	svc := NewNATSACMService(nc)

	_, err := svc.ImportCertificate(context.Background(), &acm.ImportCertificateInput{}, testAccountID)
	require.Error(t, err)
	_, err = svc.DescribeCertificate(context.Background(), &acm.DescribeCertificateInput{}, testAccountID)
	require.Error(t, err)
	_, err = svc.ListCertificates(context.Background(), &acm.ListCertificatesInput{}, testAccountID)
	require.Error(t, err)
	_, err = svc.DeleteCertificate(context.Background(), &acm.DeleteCertificateInput{}, testAccountID)
	require.Error(t, err)
	_, err = svc.ListTagsForCertificate(context.Background(), &acm.ListTagsForCertificateInput{}, testAccountID)
	require.Error(t, err)
	_, err = svc.AddTagsToCertificate(context.Background(), &acm.AddTagsToCertificateInput{}, testAccountID)
	require.Error(t, err)
	_, err = svc.RemoveTagsFromCertificate(context.Background(), &acm.RemoveTagsFromCertificateInput{}, testAccountID)
	require.Error(t, err)
}
