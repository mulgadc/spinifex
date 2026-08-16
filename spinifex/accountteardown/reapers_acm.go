package accountteardown

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// acmNATSTimeout matches the gateway's own certificate timeout.
const acmNATSTimeout = 30 * time.Second

// ACMReapers returns the certificate reaper.
//
// ACM has no service adapter, so this talks to the same subjects the gateway
// does. Platform stage: a certificate outlives the listener that referenced it,
// and nothing in the network stage blocks on one.
func ACMReapers(nc *nats.Conn) []Reaper {
	return []Reaper{&certificateReaper{nc: nc}}
}

type certificateReaper struct {
	nc *nats.Conn
}

func (r *certificateReaper) Kind() string { return "acm-certificate" }
func (r *certificateReaper) Stage() Stage { return StagePlatform }

func (r *certificateReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := utils.NATSRequest[acm.ListCertificatesOutput](ctx, r.nc, "acm.ListCertificates",
		&acm.ListCertificatesInput{}, acmNATSTimeout, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, summary := range out.CertificateSummaryList {
		if summary == nil || summary.CertificateArn == nil {
			continue
		}
		found = append(found, Resource{
			Kind:   r.Kind(),
			ID:     aws.StringValue(summary.CertificateArn),
			Detail: aws.StringValue(summary.DomainName),
		})
	}
	return found, nil
}

func (r *certificateReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := utils.NATSRequest[acm.DeleteCertificateOutput](ctx, r.nc, "acm.DeleteCertificate",
		&acm.DeleteCertificateInput{CertificateArn: aws.String(resource.ID)}, acmNATSTimeout, accountID)
	return ignoreAlreadyGone(err)
}
