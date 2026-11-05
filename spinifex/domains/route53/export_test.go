package route53

import "context"

// SeedActiveZone writes the records CreateHostedZone writes, already ACTIVE,
// without its per-call account listings, which make seeding thousands of zones
// quadratic. It returns the zone's ID in API form.
func (s *Service) SeedActiveZone(ctx context.Context, account, name, ref string) (string, error) {
	name = normalizeName(name)
	nameservers := s.Nameservers()
	now := s.Now().UTC()
	id := newZoneID()
	if _, err := s.store.ClaimCallerReference(ctx, account, ref, id); err != nil {
		return "", err
	}
	zone := &ZoneRecord{
		ID: id, AccountID: account, Name: name, CallerReference: ref,
		CreatedAt: now, Nameservers: nameservers, State: StateActive,
	}
	doc := &RRSetsDoc{ZoneID: id, Version: 1, UpdatedAt: now, Sets: defaultRecordSets(name, nameservers)}
	if err := s.store.CreateZone(ctx, zone, doc); err != nil {
		return "", err
	}
	c := &ChangeRecord{
		ID: newChangeID(), AccountID: account, ZoneID: id, Version: doc.Version,
		Status: ChangeStatusInSync, SubmittedAt: now,
	}
	if err := s.store.PutChange(ctx, c); err != nil {
		return "", err
	}
	return hostedZonePrefix + id, nil
}
