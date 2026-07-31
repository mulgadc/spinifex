package dns

import (
	"log/slog"
	"strings"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/config"
)

// HostsZone reports whether northstar hosts a zone covering domain — the zone
// for domain itself, or for one of its parent labels (so a request for
// "jellyfin.lab.example.com" matches a zone hosted at "lab.example.com"
// without requiring an exact-match zone per host).
//
// Used by ACM's deriveValidationMode to tell an internal zone (only a private
// CA or a hand-published TXT record can prove control over it) from a public
// one (a real ACME CA's own validation applies). Returns false, not an error,
// whenever the answer cannot be determined: northstar S3 zone storage is not
// configured (zoneS3Config fails), or every candidate zone lookup errors — a
// certificate request must not hard-fail because a zone lookup was
// unavailable, it must just decline to select a northstar-answered mode.
func HostsZone(cfg *config.Config, domain string) bool {
	zoneCfg, ok := zoneS3Config(cfg)
	if !ok {
		return false
	}

	domain = strings.ToLower(strings.TrimSpace(domain))
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return false
	}
	labels := strings.Split(domain, ".")

	// Walk parent labels longest-first: the most specific zone that exists
	// wins. Stop once fewer than two labels remain so a bare TLD ("com") is
	// never queried as a candidate zone on its own.
	for len(labels) >= 2 {
		zone := strings.Join(labels, ".")
		_, exists, err := nsconfig.ReadZoneRaw(zoneCfg.s3, zone)
		if err != nil {
			slog.Warn("dns: HostsZone lookup failed, treating as not hosted", "zone", zone, "error", err)
		} else if exists {
			return true
		}
		labels = labels[1:]
	}
	return false
}
