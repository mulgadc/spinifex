package route53

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// AWS default and hard quotas that bound a change batch and a zone.
const (
	maxRecordSetsPerZone       = 10_000
	maxRecordsPerChangeRequest = 1_000
	maxValueCharsPerChange     = 32_000
	maxValuesPerRecordSet      = 400
	maxTTL                     = 2_147_483_647
	defaultNSTTL               = 172_800
	defaultSOATTL              = 900
)

// supportedTypes are the record types a simple-routing record set may carry.
var supportedTypes = []string{"A", "AAAA", "CAA", "CNAME", "MX", "NS", "PTR", "SOA", "SPF", "SRV", "TXT"}

// applyChangeBatch applies batch to a copy of sets in order, as Route 53 does,
// and returns the result only if every change is valid. Every problem found is
// reported in one InvalidChangeBatch, so the caller sees them all at once.
func applyChangeBatch(zone string, sets []RRSet, batch *route53.ChangeBatch) ([]RRSet, error) {
	if batch == nil || len(batch.Changes) == 0 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "ChangeBatch must contain at least one change")
	}
	if err := checkBatchSize(batch); err != nil {
		return nil, err
	}
	out := slices.Clone(sets)
	var problems []string
	for _, ch := range batch.Changes {
		if ch == nil || ch.ResourceRecordSet == nil {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "every change needs an Action and a ResourceRecordSet")
		}
		set, err := toRRSet(zone, ch.ResourceRecordSet)
		if err != nil {
			if code, _, ok := awserrors.ResolveErrorDetail(err); ok && code != awserrors.ErrorInvalidChangeBatch {
				return nil, err
			}
			problems = append(problems, messageOf(err))
			continue
		}
		var msg string
		out, msg = applyOne(zone, out, aws.StringValue(ch.Action), set)
		if msg != "" {
			problems = append(problems, msg)
		}
	}
	if len(problems) == 0 && len(out) > maxRecordSetsPerZone {
		problems = append(problems, fmt.Sprintf("The hosted zone %s would exceed the limit of %d resource record sets", zone, maxRecordSetsPerZone))
	}
	if len(problems) > 0 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidChangeBatch, "[%s]", strings.Join(problems, ", "))
	}
	return out, nil
}

func checkBatchSize(batch *route53.ChangeBatch) error {
	records, chars := 0, 0
	for _, ch := range batch.Changes {
		if ch == nil || ch.ResourceRecordSet == nil {
			continue
		}
		weight := 1
		if aws.StringValue(ch.Action) == route53.ChangeActionUpsert {
			weight = 2
		}
		for _, rr := range ch.ResourceRecordSet.ResourceRecords {
			records += weight
			chars += weight * len(aws.StringValue(rr.Value))
		}
	}
	if records > maxRecordsPerChangeRequest {
		return awserrors.Errorf(awserrors.ErrorInvalidChangeBatch, "[Number of records limit of %d exceeded.]", maxRecordsPerChangeRequest)
	}
	if chars > maxValueCharsPerChange {
		return awserrors.Errorf(awserrors.ErrorInvalidChangeBatch, "[Number of characters limit of %d exceeded.]", maxValueCharsPerChange)
	}
	return nil
}

func applyOne(zone string, sets []RRSet, action string, set RRSet) ([]RRSet, string) {
	at := slices.IndexFunc(sets, func(s RRSet) bool { return s.Name == set.Name && s.Type == set.Type })
	label := fmt.Sprintf("[name='%s', type='%s']", set.Name, set.Type)
	apexDefault := set.Name == zone && (set.Type == "SOA" || set.Type == "NS")

	switch action {
	case route53.ChangeActionCreate:
		if at >= 0 {
			return sets, "Tried to create resource record set " + label + " but it already exists"
		}
		if msg := conflicts(zone, sets, set); msg != "" {
			return sets, msg
		}
		return append(sets, set), ""
	case route53.ChangeActionUpsert:
		if msg := conflicts(zone, sets, set); msg != "" {
			return sets, msg
		}
		if at >= 0 {
			sets[at] = set
			return sets, ""
		}
		return append(sets, set), ""
	case route53.ChangeActionDelete:
		if at < 0 {
			return sets, "Tried to delete resource record set " + label + " but it was not found"
		}
		if !sameRRSet(sets[at], set) {
			return sets, "Tried to delete resource record set " + label + " but the values provided do not match the current values"
		}
		if apexDefault {
			return sets, fmt.Sprintf("A HostedZone must contain at least one %s record for the zone itself.", set.Type)
		}
		return slices.Delete(sets, at, at+1), ""
	default:
		return sets, fmt.Sprintf("Invalid action %q for resource record set %s", action, label)
	}
}

// conflicts applies the per-name rules: a CNAME stands alone at its name and
// never at the apex, and SOA exists only at the apex.
func conflicts(zone string, sets []RRSet, set RRSet) string {
	if set.Type == "SOA" && set.Name != zone {
		return fmt.Sprintf("RRSet of type SOA with DNS name %s is not permitted because SOA records are only permitted at the zone apex", set.Name)
	}
	if set.Type == "CNAME" && set.Name == zone {
		return fmt.Sprintf("RRSet of type CNAME with DNS name %s is not permitted at apex in zone %s", set.Name, zone)
	}
	for _, s := range sets {
		if s.Name != set.Name || s.Type == set.Type {
			continue
		}
		if s.Type == "CNAME" || set.Type == "CNAME" {
			return fmt.Sprintf("RRSet of type %s with DNS name %s is not permitted because a conflicting RRSet of type %s with the same DNS name already exists in zone %s", set.Type, set.Name, s.Type, zone)
		}
	}
	return ""
}

func sameRRSet(a, b RRSet) bool {
	return a.TTL == b.TTL && slices.Equal(sortedCopy(a.Values), sortedCopy(b.Values))
}

func sortedCopy(v []string) []string {
	c := slices.Clone(v)
	slices.Sort(c)
	return c
}

// toRRSet validates one wire record set and converts it. Routing policies,
// alias targets and health checks are refused with InvalidInput.
func toRRSet(zone string, in *route53.ResourceRecordSet) (RRSet, error) {
	if in.AliasTarget != nil {
		return RRSet{}, awserrors.Errorf(awserrors.ErrorInvalidInput, "alias records are not supported")
	}
	if in.SetIdentifier != nil || in.Weight != nil || in.Region != nil || in.Failover != nil ||
		in.GeoLocation != nil || in.GeoProximityLocation != nil || in.MultiValueAnswer != nil || in.CidrRoutingConfig != nil {
		return RRSet{}, awserrors.Errorf(awserrors.ErrorInvalidInput, "only simple routing is supported; SetIdentifier and routing policies are not")
	}
	if in.HealthCheckId != nil || in.TrafficPolicyInstanceId != nil {
		return RRSet{}, awserrors.Errorf(awserrors.ErrorInvalidInput, "health checks and traffic policies are not supported")
	}
	rrType := strings.ToUpper(aws.StringValue(in.Type))
	if !slices.Contains(supportedTypes, rrType) {
		return RRSet{}, awserrors.Errorf(awserrors.ErrorInvalidInput, "record type %q is not supported", rrType)
	}
	name := normalizeName(aws.StringValue(in.Name))
	if err := validateRecordName(name, zone); err != nil {
		return RRSet{}, awserrors.Errorf(awserrors.ErrorInvalidChangeBatch, "%s", messageOf(err))
	}
	if in.TTL == nil {
		return RRSet{}, awserrors.Errorf(awserrors.ErrorInvalidChangeBatch, "Invalid request: Expected exactly one of [AliasTarget, all of [TTL, and ResourceRecords]], but found none in Change with [Name=%s, Type=%s, SetIdentifier=null]", name, rrType)
	}
	ttl := aws.Int64Value(in.TTL)
	if ttl < 0 || ttl > maxTTL {
		return RRSet{}, awserrors.Errorf(awserrors.ErrorInvalidInput, "TTL %d is out of range", ttl)
	}
	if len(in.ResourceRecords) == 0 {
		return RRSet{}, awserrors.Errorf(awserrors.ErrorInvalidInput, "ResourceRecords must contain at least one record")
	}
	if len(in.ResourceRecords) > maxValuesPerRecordSet {
		return RRSet{}, awserrors.Errorf(awserrors.ErrorInvalidChangeBatch, "RRSet %s %s has more than %d values", name, rrType, maxValuesPerRecordSet)
	}
	set := RRSet{Name: name, Type: rrType, TTL: ttl}
	for _, rr := range in.ResourceRecords {
		v := strings.TrimSpace(aws.StringValue(rr.Value))
		if err := validateValue(rrType, v); err != nil {
			return RRSet{}, awserrors.Errorf(awserrors.ErrorInvalidChangeBatch, "Invalid Resource Record: %s for %s %s", err, name, rrType)
		}
		set.Values = append(set.Values, v)
	}
	if (rrType == "CNAME" || rrType == "SOA") && len(set.Values) > 1 {
		return RRSet{}, awserrors.Errorf(awserrors.ErrorInvalidChangeBatch, "RRSet of type %s with DNS name %s has more than one value", rrType, name)
	}
	return set, nil
}

// validateValue checks a record's RDATA in its presentation format.
func validateValue(rrType, v string) error {
	if v == "" {
		return fmt.Errorf("empty value")
	}
	f := strings.Fields(v)
	switch rrType {
	case "A":
		if ip := net.ParseIP(v); ip == nil || ip.To4() == nil || strings.Contains(v, ":") {
			return fmt.Errorf("value %q is not a valid IPv4 address", v)
		}
	case "AAAA":
		if ip := net.ParseIP(v); ip == nil || !strings.Contains(v, ":") {
			return fmt.Errorf("value %q is not a valid IPv6 address", v)
		}
	case "CNAME", "NS", "PTR":
		return validHostValue(v)
	case "MX":
		if len(f) != 2 || !validUint16(f[0]) {
			return fmt.Errorf("value %q is not \"priority host\"", v)
		}
		return validHostValue(f[1])
	case "SRV":
		if len(f) != 4 || !validUint16(f[0]) || !validUint16(f[1]) || !validUint16(f[2]) {
			return fmt.Errorf("value %q is not \"priority weight port target\"", v)
		}
		return validHostValue(f[3])
	case "CAA":
		if len(f) < 3 || !validUint8(f[0]) {
			return fmt.Errorf("value %q is not \"flags tag value\"", v)
		}
	case "SOA":
		if len(f) != 7 {
			return fmt.Errorf("value %q is not a seven-field SOA", v)
		}
	}
	return nil
}

func validHostValue(v string) error {
	if v == "." {
		return nil
	}
	if _, err := splitLabels(normalizeName(v), true); err != nil {
		return fmt.Errorf("value %q is not a valid host name", v)
	}
	return nil
}

func validUint16(s string) bool {
	_, err := strconv.ParseUint(s, 10, 16)
	return err == nil
}

func validUint8(s string) bool {
	_, err := strconv.ParseUint(s, 10, 8)
	return err == nil
}

// messageOf returns the client message an awserrors error carries.
func messageOf(err error) string {
	if _, msg, ok := awserrors.ResolveErrorDetail(err); ok && msg != "" {
		return msg
	}
	return err.Error()
}
