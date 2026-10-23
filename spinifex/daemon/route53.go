package daemon

import (
	"github.com/mulgadc/spinifex/spinifex/domains/dns"
	route53domain "github.com/mulgadc/spinifex/spinifex/domains/route53"
	"github.com/nats-io/nats.go/jetstream"
)

// newRoute53Service builds the Route 53 control plane. The nameservers and the
// reserved names are read from the cluster's DNS settings on every call, so a
// node joining or a domain change needs no restart.
func (d *Daemon) newRoute53Service(js jetstream.JetStream) *route53domain.Service {
	svc := route53domain.NewService(route53domain.NewStore(js))
	svc.Nameservers = func() []string {
		base := dns.ResolveBaseDomain(d.config)
		if base == "" || d.clusterConfig == nil {
			return nil
		}
		published := dns.PublishedNameservers(d.clusterConfig)
		hosts := make([]string, 0, len(published))
		for _, ns := range published {
			hosts = append(hosts, ns.Host+"."+base)
		}
		return hosts
	}
	svc.Reserved = func(name string) bool {
		return dns.ReservedZoneName(name, dns.ResolveBaseDomain(d.config), dns.ResolveInternalDomain(d.config))
	}
	return svc
}
