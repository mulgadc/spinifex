package route53

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"

	"github.com/mulgadc/spinifex/spinifex/domains/route53/ext"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

// GetHostedZoneActivation reports whether a zone is served and why not.
func (s *Service) GetHostedZoneActivation(ctx context.Context, in *ext.GetHostedZoneActivationInput, account string) (*ext.GetHostedZoneActivationOutput, error) {
	id := BareZoneID(aws.StringValue(in.Id))
	zone, err := s.store.GetZone(ctx, account, id)
	if err != nil {
		return nil, s.zoneErr(id, err)
	}
	return &ext.GetHostedZoneActivationOutput{HostedZoneId: aws.String(hostedZonePrefix + zone.ID), Activation: activation(zone)}, nil
}

// VerifyHostedZoneDelegation checks a zone's delegation now, the console's
// "Check now", rather than at its next scheduled check.
func (s *Service) VerifyHostedZoneDelegation(ctx context.Context, in *ext.VerifyHostedZoneDelegationInput, account string) (*ext.VerifyHostedZoneDelegationOutput, error) {
	id := BareZoneID(aws.StringValue(in.Id))
	if s.Verifier == nil {
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "delegation checks are not available on this cluster")
	}
	zone, err := s.Verifier.Verify(ctx, account, id)
	if err != nil {
		return nil, s.zoneErr(id, err)
	}
	return &ext.VerifyHostedZoneDelegationOutput{HostedZoneId: aws.String(hostedZonePrefix + zone.ID), Activation: activation(zone)}, nil
}

func activation(z *ZoneRecord) *ext.Activation {
	a := &ext.Activation{
		State:            aws.String(z.State),
		Reason:           nonEmpty(z.StateReason),
		NameServers:      aws.StringSlice(z.Nameservers),
		DelegatedTo:      aws.StringSlice(z.Delegation),
		DelegationSource: nonEmpty(z.DelegationSource),
		Override:         nonEmpty(z.Override),
	}
	if !z.CheckedAt.IsZero() {
		a.CheckedAt = aws.Time(z.CheckedAt)
	}
	if z.Override != "" {
		a.Reason = nonEmpty(z.OverrideReason)
	}
	return a
}

// AdminZoneQuery selects zones across every account, by ID or by name.
type AdminZoneQuery struct {
	ZoneID string `json:"zone_id,omitempty"`
	Name   string `json:"name,omitempty"`
}

// AdminZones is the zones a query matched.
type AdminZones struct {
	Zones []ZoneRecord `json:"zones"`
}

// AdminOverrideInput sets or clears an operator override on a zone.
type AdminOverrideInput struct {
	ZoneID string `json:"zone_id"`
	// Override is StateActive, StateSuspended, or "" to hand the zone back to
	// the verifier.
	Override string `json:"override"`
	Reason   string `json:"reason"`
}

// AdminDelegateInput names the platform-zone delegations to write or, with
// Remove, withdraw.
type AdminDelegateInput struct {
	Names  []string `json:"names"`
	Remove bool     `json:"remove,omitempty"`
}

// AdminDelegateOutput reports how many record changes were applied.
type AdminDelegateOutput struct {
	Applied int `json:"applied"`
}

// AdminFindZones lists the zones matching q in every account. It is for the
// operator and is never reachable through the gateway.
func (s *Service) AdminFindZones(ctx context.Context, q *AdminZoneQuery, _ string) (*AdminZones, error) {
	zones, err := s.store.ListAllZones(ctx)
	if err != nil {
		return nil, internal("list hosted zones", err)
	}
	id, name := BareZoneID(q.ZoneID), normalizeName(q.Name)
	out := &AdminZones{Zones: []ZoneRecord{}}
	for _, z := range zones {
		if (id == "" || z.ID == id) && (name == "" || z.Name == name) {
			out.Zones = append(out.Zones, z)
		}
	}
	slices.SortFunc(out.Zones, func(a, b ZoneRecord) int { return olderFirst(&a, &b) })
	return out, nil
}

// AdminVerify checks one zone now, in whichever account holds it.
func (s *Service) AdminVerify(ctx context.Context, q *AdminZoneQuery, _ string) (*AdminZones, error) {
	z, err := s.adminZone(ctx, q.ZoneID)
	if err != nil {
		return nil, err
	}
	if s.Verifier == nil {
		return nil, awserrors.Errorf(awserrors.ErrorServiceUnavailable, "delegation checks are not available on this cluster")
	}
	checked, err := s.Verifier.Verify(ctx, z.AccountID, z.ID)
	if err != nil {
		return nil, s.zoneErr(z.ID, err)
	}
	return &AdminZones{Zones: []ZoneRecord{*checked}}, nil
}

// AdminOverride activates or suspends a zone whatever its delegation, or
// clears an override. Activation refuses a name active in another zone.
func (s *Service) AdminOverride(ctx context.Context, in *AdminOverrideInput, _ string) (*AdminZones, error) {
	if in.Override != "" && strings.TrimSpace(in.Reason) == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "an override needs a reason")
	}
	z, err := s.adminZone(ctx, in.ZoneID)
	if err != nil {
		return nil, err
	}
	switch in.Override {
	case StateActive:
		if s.Reserved != nil && s.Reserved(z.Name) {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidDomainName, "%s is reserved by this cluster and cannot be activated", strings.TrimSuffix(z.Name, "."))
		}
		zones, err := s.store.ListAllZones(ctx)
		if err != nil {
			return nil, internal("list hosted zones", err)
		}
		for _, other := range zones {
			if other.Name == z.Name && other.ID != z.ID && other.State == StateActive {
				return nil, awserrors.Errorf(awserrors.ErrorConflictingDomainExists, "%s is already active in account %s as %s", strings.TrimSuffix(z.Name, "."), other.AccountID, other.ID)
			}
		}
	case StateSuspended, "":
	default:
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "override must be %s, %s or empty", StateActive, StateSuspended)
	}
	err = s.store.MutateZone(ctx, z.AccountID, z.ID, func(r *ZoneRecord) (bool, error) {
		switch in.Override {
		case "":
			// The verifier decides again from here, starting with a check.
			if r.State == StateSuspended || r.Override == StateActive {
				r.State = StatePendingNameservers
			}
			r.CheckedAt = time.Time{}
		default:
			r.State = in.Override
		}
		r.Override, r.OverrideReason = in.Override, strings.TrimSpace(in.Reason)
		r.FailingSince = time.Time{}
		return true, nil
	})
	if err != nil {
		return nil, s.zoneErr(z.ID, err)
	}
	updated, err := s.store.GetZone(ctx, z.AccountID, z.ID)
	if err != nil {
		return nil, s.zoneErr(z.ID, err)
	}
	return &AdminZones{Zones: []ZoneRecord{*updated}}, nil
}

func (s *Service) adminZone(ctx context.Context, rawID string) (*ZoneRecord, error) {
	id := BareZoneID(rawID)
	if id == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "a hosted zone ID is required")
	}
	found, err := s.AdminFindZones(ctx, &AdminZoneQuery{ZoneID: id}, "")
	if err != nil {
		return nil, err
	}
	if len(found.Zones) == 0 {
		return nil, s.zoneErr(id, kvstore.ErrNotFound)
	}
	return &found.Zones[0], nil
}
