// Package route53 is the daemon-side Route 53 control plane: public hosted
// zones and their record sets in JetStream KV, change tracking for GetChange,
// and the activation state that decides whether northstar serves a zone.
package route53

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
)

const (
	defaultMaxZonesPerAccount = 500
	maxCallerReferenceLength  = 128
	maxCommentLength          = 256
	maxListZones              = 100
	maxListRecordSets         = 300
	maxTagsPerResource        = 50
	maxTagResources           = 10
	maxTagsPerChange          = 10
	maxTagKeyLength           = 128
	changeRetention           = 7 * 24 * time.Hour
	commitAttempts            = 3
)

// Service implements the Route 53 operations the gateway forwards over NATS.
// Methods take the account from the request, never from the input.
type Service struct {
	store *Store

	// Nameservers returns the published nameserver host names, without a
	// trailing dot, that a new zone is delegated to.
	Nameservers func() []string
	// Reserved reports whether a zone name belongs to the platform and may not
	// be created by a tenant.
	Reserved func(name string) bool
	// Now is the clock; tests replace it.
	Now func() time.Time
	// MaxZonesPerAccount caps CreateHostedZone; zero means the AWS default.
	MaxZonesPerAccount int
	// Verifier checks a delegation on demand; nil leaves zones to the schedule.
	Verifier *Verifier
}

// NewService returns a Service over store.
func NewService(store *Store) *Service {
	return &Service{store: store, Now: time.Now}
}

// Store returns the service's store, for the realizer and verifier.
func (s *Service) Store() *Store { return s.store }

func (s *Service) CreateHostedZone(ctx context.Context, in *route53.CreateHostedZoneInput, account string) (*route53.CreateHostedZoneOutput, error) {
	ref := aws.StringValue(in.CallerReference)
	if ref == "" || len(ref) > maxCallerReferenceLength {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "CallerReference must be 1 to %d characters", maxCallerReferenceLength)
	}
	if in.VPC != nil || (in.HostedZoneConfig != nil && aws.BoolValue(in.HostedZoneConfig.PrivateZone)) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "private hosted zones are not supported")
	}
	if in.DelegationSetId != nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "reusable delegation sets are not supported")
	}
	comment := ""
	if in.HostedZoneConfig != nil {
		comment = aws.StringValue(in.HostedZoneConfig.Comment)
	}
	if len(comment) > maxCommentLength {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "Comment must be at most %d characters", maxCommentLength)
	}
	name := normalizeName(aws.StringValue(in.Name))
	if err := validateZoneName(name); err != nil {
		return nil, err
	}
	if s.Reserved != nil && s.Reserved(name) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidDomainName, "%s is reserved by this cluster and cannot be a hosted zone", name)
	}
	var nameservers []string
	if s.Nameservers != nil {
		nameservers = s.Nameservers()
	}
	if len(nameservers) == 0 {
		return nil, awserrors.Errorf(awserrors.ErrorServerInternal, "no nameservers are published for this cluster")
	}

	existing, err := s.store.ListAccountZones(ctx, account)
	if err != nil {
		return nil, internal("list hosted zones", err)
	}
	limit := s.MaxZonesPerAccount
	if limit == 0 {
		limit = defaultMaxZonesPerAccount
	}
	if len(existing) >= limit {
		return nil, awserrors.Errorf(awserrors.ErrorTooManyHostedZones, "account %s already has %d hosted zones", account, len(existing))
	}

	id := newZoneID()
	held, err := s.store.ClaimCallerReference(ctx, account, ref, id)
	if err != nil {
		return nil, internal("claim caller reference", err)
	}
	if held != "" {
		if _, err := s.store.GetZone(ctx, account, held); !errors.Is(err, kvstore.ErrNotFound) {
			return nil, awserrors.Errorf(awserrors.ErrorHostedZoneAlreadyExists, "A hosted zone has already been created with the specified caller reference.")
		}
		// The earlier claim's zone was never written: take the reference over.
		if err := s.store.ReleaseCallerReference(ctx, account, ref); err != nil {
			return nil, internal("release caller reference", err)
		}
		if held, err = s.store.ClaimCallerReference(ctx, account, ref, id); err != nil || held != "" {
			return nil, awserrors.Errorf(awserrors.ErrorPriorRequestNotComplete, "another request with this caller reference is in progress")
		}
	}

	now := s.Now().UTC()
	zone := &ZoneRecord{
		ID: id, AccountID: account, Name: name, CallerReference: ref, Comment: comment,
		CreatedAt: now, Nameservers: nameservers, State: StatePendingNameservers,
	}
	doc := &RRSetsDoc{ZoneID: id, Version: 1, UpdatedAt: now, Sets: defaultRecordSets(name, nameservers)}
	if err := s.store.CreateZone(ctx, zone, doc); err != nil {
		_ = s.store.ReleaseCallerReference(ctx, account, ref)
		return nil, internal("create hosted zone", err)
	}
	change, err := s.recordChange(ctx, zone, doc.Version, "")
	if err != nil {
		return nil, err
	}
	return &route53.CreateHostedZoneOutput{
		HostedZone:    hostedZone(zone, len(doc.Sets)),
		ChangeInfo:    change,
		DelegationSet: delegationSet(zone),
		Location:      aws.String("/2013-04-01" + hostedZonePrefix + id),
	}, nil
}

func (s *Service) GetHostedZone(ctx context.Context, in *route53.GetHostedZoneInput, account string) (*route53.GetHostedZoneOutput, error) {
	zone, doc, _, err := s.zoneWithSets(ctx, account, aws.StringValue(in.Id))
	if err != nil {
		return nil, err
	}
	return &route53.GetHostedZoneOutput{HostedZone: hostedZone(zone, len(doc.Sets)), DelegationSet: delegationSet(zone)}, nil
}

func (s *Service) GetHostedZoneCount(ctx context.Context, _ *route53.GetHostedZoneCountInput, account string) (*route53.GetHostedZoneCountOutput, error) {
	zones, err := s.store.ListAccountZones(ctx, account)
	if err != nil {
		return nil, internal("list hosted zones", err)
	}
	return &route53.GetHostedZoneCountOutput{HostedZoneCount: aws.Int64(int64(len(zones)))}, nil
}

func (s *Service) ListHostedZones(ctx context.Context, in *route53.ListHostedZonesInput, account string) (*route53.ListHostedZonesOutput, error) {
	limit, err := maxItems(in.MaxItems, maxListZones)
	if err != nil {
		return nil, err
	}
	if in.DelegationSetId != nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "reusable delegation sets are not supported")
	}
	zones, err := s.sortedZones(ctx, account, func(a, b ZoneRecord) int { return strings.Compare(a.ID, b.ID) })
	if err != nil {
		return nil, err
	}
	if aws.StringValue(in.HostedZoneType) == route53.HostedZoneTypePrivateHostedZone {
		zones = nil
	}
	marker := BareZoneID(aws.StringValue(in.Marker))
	start := 0
	if marker != "" {
		start = slices.IndexFunc(zones, func(z ZoneRecord) bool { return z.ID >= marker })
		if start < 0 {
			start = len(zones)
		}
	}
	page, truncated := pageOf(zones, start, limit)
	out := &route53.ListHostedZonesOutput{
		HostedZones: s.hostedZones(ctx, page),
		IsTruncated: aws.Bool(truncated),
		MaxItems:    aws.String(strconv.Itoa(limit)),
		Marker:      in.Marker,
	}
	if out.Marker == nil {
		out.Marker = aws.String("")
	}
	if truncated {
		out.NextMarker = aws.String(zones[start+limit].ID)
	}
	return out, nil
}

func (s *Service) ListHostedZonesByName(ctx context.Context, in *route53.ListHostedZonesByNameInput, account string) (*route53.ListHostedZonesByNameOutput, error) {
	limit, err := maxItems(in.MaxItems, maxListZones)
	if err != nil {
		return nil, err
	}
	dnsName := normalizeName(aws.StringValue(in.DNSName))
	startID := BareZoneID(aws.StringValue(in.HostedZoneId))
	if startID != "" && dnsName == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "HostedZoneId requires DNSName")
	}
	zones, err := s.sortedZones(ctx, account, compareByName)
	if err != nil {
		return nil, err
	}
	start := 0
	if dnsName != "" {
		from := ZoneRecord{Name: dnsName, ID: startID}
		start = slices.IndexFunc(zones, func(z ZoneRecord) bool { return compareByName(z, from) >= 0 })
		if start < 0 {
			start = len(zones)
		}
	}
	page, truncated := pageOf(zones, start, limit)
	out := &route53.ListHostedZonesByNameOutput{
		HostedZones:  s.hostedZones(ctx, page),
		IsTruncated:  aws.Bool(truncated),
		MaxItems:     aws.String(strconv.Itoa(limit)),
		DNSName:      in.DNSName,
		HostedZoneId: in.HostedZoneId,
	}
	if truncated {
		next := zones[start+limit]
		out.NextDNSName = aws.String(next.Name)
		out.NextHostedZoneId = aws.String(next.ID)
	}
	return out, nil
}

func (s *Service) UpdateHostedZoneComment(ctx context.Context, in *route53.UpdateHostedZoneCommentInput, account string) (*route53.UpdateHostedZoneCommentOutput, error) {
	comment := aws.StringValue(in.Comment)
	if len(comment) > maxCommentLength {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "Comment must be at most %d characters", maxCommentLength)
	}
	id := BareZoneID(aws.StringValue(in.Id))
	err := s.store.MutateZone(ctx, account, id, func(z *ZoneRecord) (bool, error) {
		if z.Comment == comment {
			return false, nil
		}
		z.Comment = comment
		return true, nil
	})
	if err != nil {
		return nil, s.zoneErr(id, err)
	}
	return s.updatedZone(ctx, account, id)
}

func (s *Service) updatedZone(ctx context.Context, account, id string) (*route53.UpdateHostedZoneCommentOutput, error) {
	zone, doc, _, err := s.zoneWithSets(ctx, account, id)
	if err != nil {
		return nil, err
	}
	return &route53.UpdateHostedZoneCommentOutput{HostedZone: hostedZone(zone, len(doc.Sets))}, nil
}

func (s *Service) DeleteHostedZone(ctx context.Context, in *route53.DeleteHostedZoneInput, account string) (*route53.DeleteHostedZoneOutput, error) {
	zone, doc, rev, err := s.zoneWithSets(ctx, account, aws.StringValue(in.Id))
	if err != nil {
		return nil, err
	}
	for _, set := range doc.Sets {
		if set.Name != zone.Name || (set.Type != "SOA" && set.Type != "NS") {
			return nil, awserrors.Errorf(awserrors.ErrorHostedZoneNotEmpty, "The specified hosted zone contains non-required resource record sets and so cannot be deleted.")
		}
	}
	if err := s.store.DeleteZone(ctx, zone, rev); err != nil {
		if errors.Is(err, kvstore.ErrConflict) {
			return nil, awserrors.Errorf(awserrors.ErrorPriorRequestNotComplete, "the hosted zone changed while it was being deleted")
		}
		return nil, internal("delete hosted zone", err)
	}
	// The deletion's change outlives the zone, so GetChange can report it.
	change, err := s.recordChange(ctx, zone, doc.Version+1, "")
	if err != nil {
		return nil, err
	}
	return &route53.DeleteHostedZoneOutput{ChangeInfo: change}, nil
}

func (s *Service) ChangeResourceRecordSets(ctx context.Context, in *route53.ChangeResourceRecordSetsInput, account string) (*route53.ChangeResourceRecordSetsOutput, error) {
	var comment string
	if in.ChangeBatch != nil {
		comment = aws.StringValue(in.ChangeBatch.Comment)
	}
	if len(comment) > maxCommentLength {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "Comment must be at most %d characters", maxCommentLength)
	}
	for range commitAttempts {
		zone, doc, rev, err := s.zoneWithSets(ctx, account, aws.StringValue(in.HostedZoneId))
		if err != nil {
			return nil, err
		}
		sets, err := applyChangeBatch(zone.Name, doc.Sets, in.ChangeBatch)
		if err != nil {
			return nil, err
		}
		next := &RRSetsDoc{ZoneID: zone.ID, Version: doc.Version + 1, UpdatedAt: s.Now().UTC(), Sets: sets}
		if err := s.store.CommitRRSets(ctx, account, next, rev); err != nil {
			if errors.Is(err, kvstore.ErrConflict) {
				continue
			}
			return nil, internal("commit record sets", err)
		}
		change, err := s.recordChange(ctx, zone, next.Version, comment)
		if err != nil {
			return nil, err
		}
		return &route53.ChangeResourceRecordSetsOutput{ChangeInfo: change}, nil
	}
	return nil, awserrors.Errorf(awserrors.ErrorPriorRequestNotComplete, "the hosted zone is being changed by another request")
}

func (s *Service) ListResourceRecordSets(ctx context.Context, in *route53.ListResourceRecordSetsInput, account string) (*route53.ListResourceRecordSetsOutput, error) {
	limit, err := maxItems(in.MaxItems, maxListRecordSets)
	if err != nil {
		return nil, err
	}
	if in.StartRecordIdentifier != nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "StartRecordIdentifier is not supported: no record set has a SetIdentifier")
	}
	startType := strings.ToUpper(aws.StringValue(in.StartRecordType))
	startName := normalizeName(aws.StringValue(in.StartRecordName))
	if startType != "" && startName == "" {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "The input is not valid: StartRecordType requires StartRecordName")
	}
	_, doc, _, err := s.zoneWithSets(ctx, account, aws.StringValue(in.HostedZoneId))
	if err != nil {
		return nil, err
	}
	sets := slices.Clone(doc.Sets)
	slices.SortFunc(sets, compareRRSets)
	start := 0
	if startName != "" {
		from := RRSet{Name: startName, Type: startType}
		start = slices.IndexFunc(sets, func(r RRSet) bool { return compareRRSets(r, from) >= 0 })
		if start < 0 {
			start = len(sets)
		}
	}
	page, truncated := pageOf(sets, start, limit)
	out := &route53.ListResourceRecordSetsOutput{
		ResourceRecordSets: make([]*route53.ResourceRecordSet, 0, len(page)),
		IsTruncated:        aws.Bool(truncated),
		MaxItems:           aws.String(strconv.Itoa(limit)),
	}
	for _, r := range page {
		out.ResourceRecordSets = append(out.ResourceRecordSets, wireRRSet(r))
	}
	if truncated {
		next := sets[start+limit]
		out.NextRecordName = aws.String(escapeName(next.Name))
		out.NextRecordType = aws.String(next.Type)
	}
	return out, nil
}

func (s *Service) GetChange(ctx context.Context, in *route53.GetChangeInput, account string) (*route53.GetChangeOutput, error) {
	id := BareChangeID(aws.StringValue(in.Id))
	c, err := s.store.GetChange(ctx, account, id)
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil, awserrors.Errorf(awserrors.ErrorNoSuchChange, "A change with the specified change ID does not exist: %s", id)
	}
	if err != nil {
		return nil, internal("get change", err)
	}
	return &route53.GetChangeOutput{ChangeInfo: changeInfo(c)}, nil
}

func (s *Service) ChangeTagsForResource(ctx context.Context, in *route53.ChangeTagsForResourceInput, account string) (*route53.ChangeTagsForResourceOutput, error) {
	id, err := taggedZoneID(aws.StringValue(in.ResourceType), aws.StringValue(in.ResourceId))
	if err != nil {
		return nil, err
	}
	// A list that is sent carries 1 to 10 entries, as the model bounds it.
	if in.AddTags != nil && (len(in.AddTags) == 0 || len(in.AddTags) > maxTagsPerChange) ||
		in.RemoveTagKeys != nil && (len(in.RemoveTagKeys) == 0 || len(in.RemoveTagKeys) > maxTagsPerChange) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "AddTags and RemoveTagKeys must each hold 1 to %d entries", maxTagsPerChange)
	}
	for _, t := range in.AddTags {
		key := aws.StringValue(t.Key)
		if key == "" || len(key) > maxTagKeyLength || len(aws.StringValue(t.Value)) > 256 {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "tag keys must be 1 to 128 characters and values at most 256")
		}
	}
	for _, k := range in.RemoveTagKeys {
		if len(aws.StringValue(k)) > maxTagKeyLength {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "tag keys must be at most %d characters", maxTagKeyLength)
		}
	}
	err = s.store.MutateZone(ctx, account, id, func(z *ZoneRecord) (bool, error) {
		if z.Tags == nil {
			z.Tags = map[string]string{}
		}
		for _, k := range in.RemoveTagKeys {
			delete(z.Tags, aws.StringValue(k))
		}
		for _, t := range in.AddTags {
			z.Tags[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
		}
		if len(z.Tags) > maxTagsPerResource {
			return false, awserrors.Errorf(awserrors.ErrorRoute53TooManyTagsForZone, "a hosted zone can have at most %d tags", maxTagsPerResource)
		}
		return true, nil
	})
	if err != nil {
		return nil, s.zoneErr(id, err)
	}
	return &route53.ChangeTagsForResourceOutput{}, nil
}

func (s *Service) ListTagsForResource(ctx context.Context, in *route53.ListTagsForResourceInput, account string) (*route53.ListTagsForResourceOutput, error) {
	id, err := taggedZoneID(aws.StringValue(in.ResourceType), aws.StringValue(in.ResourceId))
	if err != nil {
		return nil, err
	}
	zone, err := s.store.GetZone(ctx, account, id)
	if err != nil {
		return nil, s.zoneErr(id, err)
	}
	return &route53.ListTagsForResourceOutput{ResourceTagSet: tagSet(zone)}, nil
}

func (s *Service) ListTagsForResources(ctx context.Context, in *route53.ListTagsForResourcesInput, account string) (*route53.ListTagsForResourcesOutput, error) {
	if len(in.ResourceIds) == 0 || len(in.ResourceIds) > maxTagResources {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "ResourceIds must name 1 to %d resources", maxTagResources)
	}
	out := &route53.ListTagsForResourcesOutput{ResourceTagSets: []*route53.ResourceTagSet{}}
	for _, rid := range in.ResourceIds {
		id, err := taggedZoneID(aws.StringValue(in.ResourceType), aws.StringValue(rid))
		if err != nil {
			return nil, err
		}
		zone, err := s.store.GetZone(ctx, account, id)
		if err != nil {
			return nil, s.zoneErr(id, err)
		}
		out.ResourceTagSets = append(out.ResourceTagSets, tagSet(zone))
	}
	return out, nil
}

// zoneWithSets loads an account's zone and its record-set document.
func (s *Service) zoneWithSets(ctx context.Context, account, rawID string) (*ZoneRecord, *RRSetsDoc, uint64, error) {
	id := BareZoneID(rawID)
	if id == "" {
		return nil, nil, 0, awserrors.Errorf(awserrors.ErrorInvalidInput, "a hosted zone ID is required")
	}
	zone, err := s.store.GetZone(ctx, account, id)
	if err != nil {
		return nil, nil, 0, s.zoneErr(id, err)
	}
	doc, rev, err := s.store.GetRRSets(ctx, account, id)
	if err != nil {
		return nil, nil, 0, s.zoneErr(id, err)
	}
	return zone, doc, rev, nil
}

func (s *Service) zoneErr(id string, err error) error {
	if errors.Is(err, kvstore.ErrNotFound) {
		return awserrors.Errorf(awserrors.ErrorNoSuchHostedZone, "No hosted zone found with ID: %s", id)
	}
	if _, _, ok := awserrors.ResolveErrorDetail(err); ok {
		return err
	}
	return internal("read hosted zone", err)
}

// recordChange stores the change a write produced. A zone that is not being
// served is in sync as soon as the write is durable; an active one waits for
// every northstar server to load the version.
func (s *Service) recordChange(ctx context.Context, zone *ZoneRecord, version uint64, comment string) (*route53.ChangeInfo, error) {
	now := s.Now().UTC()
	c := &ChangeRecord{
		ID: newChangeID(), AccountID: zone.AccountID, ZoneID: zone.ID, Version: version,
		Status: ChangeStatusInSync, Comment: comment, SubmittedAt: now,
	}
	if zone.State == StateActive {
		c.Status = ChangeStatusPending
	}
	if err := s.store.PutChange(ctx, c); err != nil {
		return nil, internal("record change", err)
	}
	_ = s.store.PruneChanges(ctx, zone.AccountID, zone.ID, now.Add(-changeRetention), false)
	return changeInfo(c), nil
}

func (s *Service) sortedZones(ctx context.Context, account string, cmp func(a, b ZoneRecord) int) ([]ZoneRecord, error) {
	zones, err := s.store.ListAccountZones(ctx, account)
	if err != nil {
		return nil, internal("list hosted zones", err)
	}
	slices.SortFunc(zones, cmp)
	return zones, nil
}

func (s *Service) hostedZones(ctx context.Context, zones []ZoneRecord) []*route53.HostedZone {
	out := make([]*route53.HostedZone, 0, len(zones))
	for i := range zones {
		count := 0
		if doc, _, err := s.store.GetRRSets(ctx, zones[i].AccountID, zones[i].ID); err == nil {
			count = len(doc.Sets)
		}
		out = append(out, hostedZone(&zones[i], count))
	}
	return out
}

func compareByName(a, b ZoneRecord) int {
	if c := strings.Compare(reversedName(a.Name), reversedName(b.Name)); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

func compareRRSets(a, b RRSet) int {
	if c := strings.Compare(reversedName(a.Name), reversedName(b.Name)); c != 0 {
		return c
	}
	return strings.Compare(a.Type, b.Type)
}

func pageOf[T any](items []T, start, limit int) ([]T, bool) {
	end := min(start+limit, len(items))
	return items[start:end], end < len(items)
}

func maxItems(raw *string, ceiling int) (int, error) {
	if raw == nil || *raw == "" {
		return ceiling, nil
	}
	n, err := strconv.Atoi(*raw)
	if err != nil || n < 1 {
		return 0, awserrors.Errorf(awserrors.ErrorInvalidInput, "MaxItems must be a positive integer")
	}
	return min(n, ceiling), nil
}

func taggedZoneID(resourceType, id string) (string, error) {
	switch resourceType {
	case route53.TagResourceTypeHostedzone:
		return BareZoneID(id), nil
	case route53.TagResourceTypeHealthcheck:
		return "", awserrors.Errorf(awserrors.ErrorRoute53NoSuchHealthCheck, "No health check exists with the ID %s", id)
	default:
		return "", awserrors.Errorf(awserrors.ErrorInvalidInput, "ResourceType must be hostedzone or healthcheck")
	}
}

func defaultRecordSets(zone string, nameservers []string) []RRSet {
	ns := make([]string, 0, len(nameservers))
	for _, n := range nameservers {
		ns = append(ns, n+".")
	}
	hostmaster := "hostmaster." + zone
	if _, parent, ok := strings.Cut(nameservers[0], "."); ok {
		hostmaster = "hostmaster." + parent + "."
	}
	soa := fmt.Sprintf("%s %s 1 7200 900 1209600 86400", ns[0], hostmaster)
	return []RRSet{
		{Name: zone, Type: "NS", TTL: defaultNSTTL, Values: ns},
		{Name: zone, Type: "SOA", TTL: defaultSOATTL, Values: []string{soa}},
	}
}

func hostedZone(z *ZoneRecord, count int) *route53.HostedZone {
	return &route53.HostedZone{
		Id:              aws.String(hostedZonePrefix + z.ID),
		Name:            aws.String(z.Name),
		CallerReference: aws.String(z.CallerReference),
		Config: &route53.HostedZoneConfig{
			Comment:     nonEmpty(z.Comment),
			PrivateZone: aws.Bool(false),
		},
		ResourceRecordSetCount: aws.Int64(int64(count)),
	}
}

func delegationSet(z *ZoneRecord) *route53.DelegationSet {
	return &route53.DelegationSet{NameServers: aws.StringSlice(z.Nameservers)}
}

func changeInfo(c *ChangeRecord) *route53.ChangeInfo {
	submitted := c.SubmittedAt
	return &route53.ChangeInfo{
		Id:          aws.String(changePrefix + c.ID),
		Status:      aws.String(c.Status),
		SubmittedAt: &submitted,
		Comment:     nonEmpty(c.Comment),
	}
}

func tagSet(z *ZoneRecord) *route53.ResourceTagSet {
	keys := make([]string, 0, len(z.Tags))
	for k := range z.Tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	tags := make([]*route53.Tag, 0, len(keys))
	for _, k := range keys {
		tags = append(tags, &route53.Tag{Key: aws.String(k), Value: aws.String(z.Tags[k])})
	}
	return &route53.ResourceTagSet{
		ResourceType: aws.String(route53.TagResourceTypeHostedzone),
		ResourceId:   aws.String(z.ID),
		Tags:         tags,
	}
}

func wireRRSet(r RRSet) *route53.ResourceRecordSet {
	out := &route53.ResourceRecordSet{Name: aws.String(escapeName(r.Name)), Type: aws.String(r.Type), TTL: aws.Int64(r.TTL)}
	for _, v := range r.Values {
		out.ResourceRecords = append(out.ResourceRecords, &route53.ResourceRecord{Value: aws.String(v)})
	}
	return out
}

// escapeName renders a wildcard label the way Route 53 returns it, as \052.
func escapeName(name string) string {
	if strings.HasPrefix(name, "*.") {
		return `\052` + name[1:]
	}
	return name
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

func internal(what string, err error) error {
	return awserrors.Errorf(awserrors.ErrorServerInternal, "%s: %v", what, err)
}
