package dns

import (
	"fmt"
	"strings"
)

// dashIP renders an IP for AWS-style hostnames (1.2.3.4 → 1-2-3-4).
func dashIP(ip string) string {
	return strings.ReplaceAll(ip, ".", "-")
}

// EC2PublicName is the public AWS-shaped name for an instance:
// ec2-{dashed-public-ip}.{region}.compute.{baseDomain}.
func EC2PublicName(publicIP, region, baseDomain string) string {
	return fmt.Sprintf("ec2-%s.%s.compute.%s", dashIP(publicIP), region, baseDomain)
}

// EC2PrivateName is the private AWS-parity name for an instance:
// ip-{dashed-private-ip}.{region}.{internalDomain} (IMDS synthHostname). The
// internal domain defaults to PrivateZone when empty.
func EC2PrivateName(privateIP, region, internalDomain string) string {
	return fmt.Sprintf("ip-%s.%s.%s", dashIP(privateIP), region, privateZoneOrDefault(internalDomain))
}

// EC2DNSNames returns the public and private AWS-shaped DNS names for an
// instance. Each is empty when its inputs are unavailable: the public name needs
// a public IP and base domain, the private name needs a private IP. region is
// required for both.
func EC2DNSNames(region, baseDomain, internalDomain, publicIP, privateIP string) (public, private string) {
	if region == "" {
		return "", ""
	}
	if publicIP != "" && baseDomain != "" {
		public = EC2PublicName(publicIP, region, baseDomain)
	}
	if privateIP != "" {
		private = EC2PrivateName(privateIP, region, internalDomain)
	}
	return public, private
}

// EC2Changes builds the record-set changes for one instance's public and
// private addresses. Empty IPs are skipped (e.g. no public IP assigned). The
// private record lands in internalDomain (default compute.internal).
func EC2Changes(action Action, region, baseDomain, internalDomain, publicIP, privateIP string) []Change {
	var changes []Change
	if region == "" {
		return changes
	}
	if publicIP != "" && baseDomain != "" {
		changes = append(changes, Change{
			Action: action,
			Zone:   baseDomain,
			Name:   EC2PublicName(publicIP, region, baseDomain),
			Type:   "A",
			Value:  publicIP,
		})
	}
	if privateIP != "" {
		zone := privateZoneOrDefault(internalDomain)
		changes = append(changes, Change{
			Action: action,
			Zone:   zone,
			Name:   EC2PrivateName(privateIP, region, zone),
			Type:   "A",
			Value:  privateIP,
		})
	}
	return changes
}

// privateZoneOrDefault returns the configured internal domain or the
// compute.internal default when unset.
func privateZoneOrDefault(internalDomain string) string {
	if d := strings.TrimSpace(internalDomain); d != "" {
		return d
	}
	return PrivateZone
}

// relativeLabel converts a fully-qualified name to a zone-relative label in the
// form Northstar's reader expects (label + zone + "." = FQDN). The zone apex
// returns "". Names not under the zone are returned as a trailing-dot label
// defensively.
func relativeLabel(fqdn, zone string) string {
	name := strings.TrimSuffix(strings.ToLower(fqdn), ".")
	z := strings.TrimSuffix(strings.ToLower(zone), ".")
	if name == z {
		return ""
	}
	suffix := "." + z
	if strings.HasSuffix(name, suffix) {
		return name[:len(name)-len(suffix)] + "."
	}
	return name + "."
}
