package handlers_acm

import "github.com/aws/aws-sdk-go/service/acm"

// ACMService is the minimal AWS Certificate Manager-alike control plane: BYO
// certificate import and lifecycle. It covers only what ELBv2 HTTPS listeners
// need to resolve a Certificates[].CertificateArn to PEM material. Request
// flows (RequestCertificate + DNS/email validation), renewal, export, and
// tagging are intentionally out of scope for now.
type ACMService interface {
	ImportCertificate(input *acm.ImportCertificateInput, accountID string) (*acm.ImportCertificateOutput, error)
	DescribeCertificate(input *acm.DescribeCertificateInput, accountID string) (*acm.DescribeCertificateOutput, error)
	ListCertificates(input *acm.ListCertificatesInput, accountID string) (*acm.ListCertificatesOutput, error)
	DeleteCertificate(input *acm.DeleteCertificateInput, accountID string) (*acm.DeleteCertificateOutput, error)
}
