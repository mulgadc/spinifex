package handlers_route53

import (
	"time"

	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// defaultTimeout governs request-side timeouts on dns.* RPCs. Mirrors
// the EKS/IAM/STS handler conventions.
const defaultTimeout = 30 * time.Second

// NATS subject names used by gateway → daemon dispatch. Kept as
// exported constants so callers (subscribers + publishers) share one
// definition. Per-zone queue group `route53-zone-{zoneID}` for serialized
// recordset writes is wired in 1c.
const (
	SubjectCreateHostedZone         = "dns.CreateHostedZone"
	SubjectGetHostedZone            = "dns.GetHostedZone"
	SubjectListHostedZones          = "dns.ListHostedZones"
	SubjectUpdateHostedZoneComment  = "dns.UpdateHostedZoneComment"
	SubjectDeleteHostedZone         = "dns.DeleteHostedZone"
	SubjectChangeResourceRecordSets = "dns.ChangeResourceRecordSets"
	SubjectListResourceRecordSets   = "dns.ListResourceRecordSets"
	SubjectGetChange                = "dns.GetChange"

	// SubjectZoneLoadedPrefix is the prefix Eclipso publishes after each
	// successful zone reload — full subject = SubjectZoneLoadedPrefix + zoneID.
	// Loaded-version tracker subscribes here in 1c (D2 in route53-v0.md).
	SubjectZoneLoadedPrefix = "dns.zone.loaded."
)

// NATSRoute53Service is the gateway-side adapter that forwards each
// Route53Service method as a NATS request to the daemon's subscriber.
type NATSRoute53Service struct {
	natsConn *nats.Conn
}

var _ Route53Service = (*NATSRoute53Service)(nil)

// NewNATSRoute53Service returns a Route53Service backed by NATS RPC.
func NewNATSRoute53Service(conn *nats.Conn) Route53Service {
	return &NATSRoute53Service{natsConn: conn}
}

// --- Hosted zones ---

func (s *NATSRoute53Service) CreateHostedZone(input *route53.CreateHostedZoneInput, accountID string) (*route53.CreateHostedZoneOutput, error) {
	return utils.NATSRequest[route53.CreateHostedZoneOutput](s.natsConn, SubjectCreateHostedZone, input, defaultTimeout, accountID)
}

func (s *NATSRoute53Service) GetHostedZone(input *route53.GetHostedZoneInput, accountID string) (*route53.GetHostedZoneOutput, error) {
	return utils.NATSRequest[route53.GetHostedZoneOutput](s.natsConn, SubjectGetHostedZone, input, defaultTimeout, accountID)
}

func (s *NATSRoute53Service) ListHostedZones(input *route53.ListHostedZonesInput, accountID string) (*route53.ListHostedZonesOutput, error) {
	return utils.NATSRequest[route53.ListHostedZonesOutput](s.natsConn, SubjectListHostedZones, input, defaultTimeout, accountID)
}

func (s *NATSRoute53Service) UpdateHostedZoneComment(input *route53.UpdateHostedZoneCommentInput, accountID string) (*route53.UpdateHostedZoneCommentOutput, error) {
	return utils.NATSRequest[route53.UpdateHostedZoneCommentOutput](s.natsConn, SubjectUpdateHostedZoneComment, input, defaultTimeout, accountID)
}

func (s *NATSRoute53Service) DeleteHostedZone(input *route53.DeleteHostedZoneInput, accountID string) (*route53.DeleteHostedZoneOutput, error) {
	return utils.NATSRequest[route53.DeleteHostedZoneOutput](s.natsConn, SubjectDeleteHostedZone, input, defaultTimeout, accountID)
}

// --- Resource record sets + GetChange ---

func (s *NATSRoute53Service) ChangeResourceRecordSets(input *route53.ChangeResourceRecordSetsInput, accountID string) (*route53.ChangeResourceRecordSetsOutput, error) {
	return utils.NATSRequest[route53.ChangeResourceRecordSetsOutput](s.natsConn, SubjectChangeResourceRecordSets, input, defaultTimeout, accountID)
}

func (s *NATSRoute53Service) ListResourceRecordSets(input *route53.ListResourceRecordSetsInput, accountID string) (*route53.ListResourceRecordSetsOutput, error) {
	return utils.NATSRequest[route53.ListResourceRecordSetsOutput](s.natsConn, SubjectListResourceRecordSets, input, defaultTimeout, accountID)
}

func (s *NATSRoute53Service) GetChange(input *route53.GetChangeInput, accountID string) (*route53.GetChangeOutput, error) {
	return utils.NATSRequest[route53.GetChangeOutput](s.natsConn, SubjectGetChange, input, defaultTimeout, accountID)
}
