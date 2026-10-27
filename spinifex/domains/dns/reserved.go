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
// name. It refuses the base, internal and service-endpoint domains, any ancestor
// of them, every name below the internal and service-endpoint domains, and any
// name below the base domain under a platform-managed label or a nameserver.
func ReservedZoneName(name, baseDomain, internalDomain, servicesDomain string) bool {
	name = normalizeZone(name)
	if name == "" {
		return true
	}
	internal := normalizeZone(privateZoneOrDefault(internalDomain))
	services := normalizeZone(servicesDomain)
	for _, zone := range []string{normalizeZone(baseDomain), internal, services} {
		if zone == "" {
			continue
		}
		if name == zone || strings.HasSuffix(zone, "."+name) {
			return true
		}
	}
	for _, zone := range []string{internal, services} {
		if zone != "" && strings.HasSuffix(name, "."+zone) {
			return true
		}
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
