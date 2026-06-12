package handlers_acm

import "github.com/aws/aws-sdk-go/service/acm"

// ACMService covers BYO certificate import and lifecycle for ELBv2 HTTPS listeners.
type ACMService interface {
	ImportCertificate(input *acm.ImportCertificateInput, accountID string) (*acm.ImportCertificateOutput, error)
	DescribeCertificate(input *acm.DescribeCertificateInput, accountID string) (*acm.DescribeCertificateOutput, error)
	ListCertificates(input *acm.ListCertificatesInput, accountID string) (*acm.ListCertificatesOutput, error)
	DeleteCertificate(input *acm.DeleteCertificateInput, accountID string) (*acm.DeleteCertificateOutput, error)
}
