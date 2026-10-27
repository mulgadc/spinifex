package route53

import (
	"slices"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// Route 53 accepts names up to 1024 characters with escapes; the decoded DNS
// limits below are the ones that matter to a resolver.
const (
	maxDomainLength = 253
	maxLabelLength  = 63
)

// normalizeName lowercases a DNS name and gives it a trailing dot, the form
// Route 53 returns names in. An escaped "\052" leading label is "*".
func normalizeName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(n, `\052.`) {
		n = "*" + n[len(`\052`):]
	}
	if n != "" && !strings.HasSuffix(n, ".") {
		n += "."
	}
	return n
}

// validateZoneName checks a normalized hosted-zone name. A zone needs at least
// two labels: a bare TLD is not a domain a customer can delegate to us.
func validateZoneName(name string) error {
	labels, err := splitLabels(name, false)
	if err != nil {
		return err
	}
	if len(labels) < 2 {
		return awserrors.Errorf(awserrors.ErrorInvalidDomainName, "%s is a top-level domain and cannot be a hosted zone", name)
	}
	return nil
}

// validateRecordName checks a normalized record name and that it lies within
// zone. A wildcard is allowed only as the whole leftmost label.
func validateRecordName(name, zone string) error {
	if _, err := splitLabels(name, true); err != nil {
		return err
	}
	if !withinZone(name, zone) {
		return awserrors.Errorf(awserrors.ErrorInvalidChangeBatch, "RRSet with DNS name %s is not permitted in zone %s", name, zone)
	}
	return nil
}

func splitLabels(name string, allowWildcard bool) ([]string, error) {
	trimmed := strings.TrimSuffix(name, ".")
	if trimmed == "" || len(trimmed) > maxDomainLength {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidDomainName, "%q is not a valid domain name", name)
	}
	labels := strings.Split(trimmed, ".")
	for i, l := range labels {
		if l == "*" && allowWildcard && i == 0 {
			continue
		}
		if !validLabel(l) {
			return nil, awserrors.Errorf(awserrors.ErrorInvalidDomainName, "%q is not a valid domain name", name)
		}
	}
	return labels, nil
}

// validLabel accepts letters, digits, hyphens and underscores, the last for
// service labels such as _acme-challenge and _sip._tcp.
func validLabel(l string) bool {
	if l == "" || len(l) > maxLabelLength || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for _, c := range l {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// withinZone reports whether name is zone or a name below it.
func withinZone(name, zone string) bool {
	return name == zone || strings.HasSuffix(name, "."+zone)
}

// reversedName orders names the way Route 53 lists them: by label, from the
// rightmost, so a zone's apex sorts before everything under it.
func reversedName(name string) string {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	slices.Reverse(labels)
	return strings.Join(labels, ".")
}
