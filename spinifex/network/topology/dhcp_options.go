package topology

import (
	"fmt"

	handlers_imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
)

// BuildSubnetDHCPOptions returns the OVN DHCPOptions Options map emitted for a
// subnet's DHCPOptions row. Both the live topology manager and the reconciler
// call it so the two paths cannot drift — they previously hard-coded divergent
// dns_server values (live used the configured server, the reconciler "8.8.8.8").
//
// classless_static_route (RFC 3442 option 121) carries two routes: the default
// route and a /32 for the IMDS endpoint, both via the subnet gateway. The
// default route is repeated because a client honouring option 121 MUST ignore
// option 3 (router) — without it those clients would silently lose their
// gateway, matching AWS DHCP behaviour. The /32 forces the guest to route IMDS
// traffic to the subnet LRP regardless of any auto-installed 169.254.0.0/16
// link-scope route.
func BuildSubnetDHCPOptions(gwIP, routerMAC, dnsServer string) map[string]string {
	return map[string]string{
		"server_id":  gwIP,
		"server_mac": routerMAC,
		"lease_time": "3600",
		"router":     gwIP,
		"dns_server": dnsServer,
		"mtu":        "1442",
		"classless_static_route": fmt.Sprintf(
			"{0.0.0.0/0,%s, %s/32,%s}", gwIP, handlers_imds.MetaDataServerIP, gwIP,
		),
	}
}
