package dns

import (
	"regexp"
	"strings"
)

// managedLabels are the labels directly below the base domain under which the
// platform writes its own records.
var managedLabels = map[string]bool{"compute": true, "elb": true, "eks": true, "rds": true}

var nameserverLabel = regexp.MustCompile(`^ns[0-9]+$`)

// ReservedZoneName reports whether a tenant may not create a hosted zone named
// name. It refuses the base and internal domains, any ancestor of them, every
// name below the internal domain, and any name below the base domain that falls
// under a platform-managed label or a published nameserver.
func ReservedZoneName(name, baseDomain, internalDomain string) bool {
	name = normalizeZone(name)
	if name == "" {
		return true
	}
	for _, zone := range []string{normalizeZone(baseDomain), normalizeZone(privateZoneOrDefault(internalDomain))} {
		if zone == "" {
			continue
		}
		if name == zone || strings.HasSuffix(zone, "."+name) {
			return true
		}
	}
	if internal := normalizeZone(privateZoneOrDefault(internalDomain)); strings.HasSuffix(name, "."+internal) {
		return true
	}
	if base := normalizeZone(baseDomain); base != "" && strings.HasSuffix(name, "."+base) {
		labels := strings.Split(strings.TrimSuffix(name, "."+base), ".")
		top := labels[len(labels)-1]
		return managedLabels[top] || nameserverLabel.MatchString(top)
	}
	return false
}

func normalizeZone(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}
