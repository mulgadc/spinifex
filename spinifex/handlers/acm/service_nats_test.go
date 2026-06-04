package handlers_acm

import (
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

	_, err := svc.ImportCertificate(&acm.ImportCertificateInput{}, testAccountID)
	require.Error(t, err)
	_, err = svc.DescribeCertificate(&acm.DescribeCertificateInput{}, testAccountID)
	require.Error(t, err)
	_, err = svc.ListCertificates(&acm.ListCertificatesInput{}, testAccountID)
	require.Error(t, err)
	_, err = svc.DeleteCertificate(&acm.DeleteCertificateInput{}, testAccountID)
	require.Error(t, err)
}
