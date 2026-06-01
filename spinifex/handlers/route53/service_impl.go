package handlers_route53

import (
	"errors"

	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
)

// Route53ServiceImpl is the daemon-side Route53Service. Sprint 1a wires
// the struct + interface satisfaction; methods return NotImplemented
// until 1b/1c. Predastore + Eclipso reload-event tracker hooks land
// alongside their respective sprints.
type Route53ServiceImpl struct {
	nc *nats.Conn
}

var _ Route53Service = (*Route53ServiceImpl)(nil)

// NewRoute53ServiceImplWithNATS constructs the daemon-side service. The
// NATS connection is the same shared handle used by the rest of the
// daemon; predastore access is wired in 1b via a separate constructor
// arg once the dns-zones bucket exists.
func NewRoute53ServiceImplWithNATS(nc *nats.Conn) *Route53ServiceImpl {
	return &Route53ServiceImpl{nc: nc}
}

func notImpl() error { return errors.New(awserrors.ErrorNotImplemented) }

// --- Hosted zones (Sprint 1b) ---

func (s *Route53ServiceImpl) CreateHostedZone(_ *route53.CreateHostedZoneInput, _ string) (*route53.CreateHostedZoneOutput, error) {
	return nil, notImpl()
}

func (s *Route53ServiceImpl) GetHostedZone(_ *route53.GetHostedZoneInput, _ string) (*route53.GetHostedZoneOutput, error) {
	return nil, notImpl()
}

func (s *Route53ServiceImpl) ListHostedZones(_ *route53.ListHostedZonesInput, _ string) (*route53.ListHostedZonesOutput, error) {
	return nil, notImpl()
}

func (s *Route53ServiceImpl) UpdateHostedZoneComment(_ *route53.UpdateHostedZoneCommentInput, _ string) (*route53.UpdateHostedZoneCommentOutput, error) {
	return nil, notImpl()
}

func (s *Route53ServiceImpl) DeleteHostedZone(_ *route53.DeleteHostedZoneInput, _ string) (*route53.DeleteHostedZoneOutput, error) {
	return nil, notImpl()
}

// --- Resource record sets + change tracking (Sprint 1c) ---

func (s *Route53ServiceImpl) ChangeResourceRecordSets(_ *route53.ChangeResourceRecordSetsInput, _ string) (*route53.ChangeResourceRecordSetsOutput, error) {
	return nil, notImpl()
}

func (s *Route53ServiceImpl) ListResourceRecordSets(_ *route53.ListResourceRecordSetsInput, _ string) (*route53.ListResourceRecordSetsOutput, error) {
	return nil, notImpl()
}

func (s *Route53ServiceImpl) GetChange(_ *route53.GetChangeInput, _ string) (*route53.GetChangeOutput, error) {
	return nil, notImpl()
}
