package route53

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/miekg/dns"
	nsconfig "github.com/mulgadc/northstar/pkg/config"
)

// renderZone turns a zone's record sets into the northstar zone document. The
// output depends only on its inputs, so the same version always renders to
// the same bytes and its digest identifies what northstar should be serving.
//
// The SOA record set goes into the zone header rather than the records, which
// is where northstar reads the SOA it serves.
func renderZone(zone *ZoneRecord, doc *RRSetsDoc) ([]byte, error) {
	apex := strings.TrimSuffix(zone.Name, ".")
	cfg := nsconfig.ConfigArr{
		Version: 1.0,
		Domain: nsconfig.Domain{
			Domain:   apex,
			Modified: doc.UpdatedAt.UTC(),
			Active:   true,
		},
		Defaults: nsconfig.Defaults{TTL: 300, Type: dns.TypeA, Class: dns.ClassINET},
	}
	sets := slices.Clone(doc.Sets)
	slices.SortFunc(sets, func(a, b RRSet) int {
		return cmp.Or(cmp.Compare(reversedName(a.Name), reversedName(b.Name)), cmp.Compare(a.Type, b.Type))
	})
	for _, set := range sets {
		if set.Type == "SOA" {
			if f := strings.Fields(set.Values[0]); len(f) > 0 {
				cfg.Domain.SOA = f[0]
			}
			cfg.Domain.SOARecord = set.Values[0]
			cfg.Domain.SOATTL = uint32(set.TTL) //nolint:gosec // TTL is validated to at most maxTTL
			continue
		}
		rtype, ok := dns.StringToType[set.Type]
		if !ok {
			return nil, fmt.Errorf("record set %s has unknown type %s", set.Name, set.Type)
		}
		label := relativeLabel(set.Name, zone.Name)
		for _, v := range set.Values {
			rec, err := renderRecord(label, rtype, uint32(set.TTL), v) //nolint:gosec // TTL is validated to at most maxTTL
			if err != nil {
				return nil, fmt.Errorf("record set %s %s: %w", set.Name, set.Type, err)
			}
			cfg.Records = append(cfg.Records, rec)
		}
	}
	return nsconfig.RenderZone(cfg)
}

func renderRecord(label string, rtype uint16, ttl uint32, v string) (nsconfig.Records, error) {
	rec := nsconfig.Records{Domain: label, Type: rtype, Class: dns.ClassINET, TTL: ttl}
	f := strings.Fields(v)
	switch rtype {
	case dns.TypeMX:
		rec.Preference = parseUint16(f[0])
		rec.Address = dns.Fqdn(f[1])
	case dns.TypeSRV:
		rec.Priority, rec.Weight, rec.Port = parseUint16(f[0]), parseUint16(f[1]), parseUint16(f[2])
		rec.Address = dns.Fqdn(f[3])
	case dns.TypeCAA:
		flag, _ := strconv.ParseUint(f[0], 10, 8)
		rec.CAAFlag = uint8(flag)
		rec.CAATag = f[1]
		rec.Address = strings.Trim(afterFields(v, 2), `"`)
	case dns.TypeTXT, dns.TypeSPF:
		strs, err := characterStrings(v)
		if err != nil {
			return rec, err
		}
		rec.Strings = strs
	case dns.TypeCNAME, dns.TypeNS, dns.TypePTR:
		// Route 53 reads a host without the trailing dot as fully qualified, and
		// northstar cannot pack a name that is not.
		rec.Address = dns.Fqdn(v)
	default:
		rec.Address = v
	}
	return rec, nil
}

// afterFields returns v with its first n whitespace-separated fields removed.
func afterFields(v string, n int) string {
	rest := strings.TrimSpace(v)
	for range n {
		i := strings.IndexAny(rest, " \t")
		if i < 0 {
			return ""
		}
		rest = strings.TrimSpace(rest[i:])
	}
	return rest
}

func parseUint16(s string) uint16 {
	n, _ := strconv.ParseUint(s, 10, 16)
	return uint16(n)
}

// relativeLabel is name relative to zone in northstar's form: "" for the apex
// and "www." for www.<zone>.
func relativeLabel(name, zone string) string {
	if name == zone {
		return ""
	}
	return strings.TrimSuffix(name, zone)
}
