package handlers_route53

import "github.com/aws/aws-sdk-go/service/route53"

// Route53Service is the AWS Route53 contract exposed by Spinifex.
// Sprint 1a only declares the surface; all methods return
// awserrors.ErrorNotImplemented. Real bodies land in 1b (hosted zone
// CRUD) and 1c (resource-record-set CRUD + GetChange).
type Route53Service interface {
	// Hosted zone CRUD (Sprint 1b)
	CreateHostedZone(input *route53.CreateHostedZoneInput, accountID string) (*route53.CreateHostedZoneOutput, error)
	GetHostedZone(input *route53.GetHostedZoneInput, accountID string) (*route53.GetHostedZoneOutput, error)
	ListHostedZones(input *route53.ListHostedZonesInput, accountID string) (*route53.ListHostedZonesOutput, error)
	UpdateHostedZoneComment(input *route53.UpdateHostedZoneCommentInput, accountID string) (*route53.UpdateHostedZoneCommentOutput, error)
	DeleteHostedZone(input *route53.DeleteHostedZoneInput, accountID string) (*route53.DeleteHostedZoneOutput, error)

	// Resource-record-set CRUD + change tracking (Sprint 1c)
	ChangeResourceRecordSets(input *route53.ChangeResourceRecordSetsInput, accountID string) (*route53.ChangeResourceRecordSetsOutput, error)
	ListResourceRecordSets(input *route53.ListResourceRecordSetsInput, accountID string) (*route53.ListResourceRecordSetsOutput, error)
	GetChange(input *route53.GetChangeInput, accountID string) (*route53.GetChangeOutput, error)
}
