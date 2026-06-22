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
// ip-{dashed-private-ip}.{region}.compute.internal (IMDS synthHostname).
func EC2PrivateName(privateIP, region string) string {
	return fmt.Sprintf("ip-%s.%s.compute.internal", dashIP(privateIP), region)
}

// EC2Changes builds the record-set changes for one instance's public and
// private addresses. Empty IPs are skipped (e.g. no public IP assigned).
func EC2Changes(action Action, region, baseDomain, publicIP, privateIP string) []Change {
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
		changes = append(changes, Change{
			Action: action,
			Zone:   PrivateZone,
			Name:   EC2PrivateName(privateIP, region),
			Type:   "A",
			Value:  privateIP,
		})
	}
	return changes
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
