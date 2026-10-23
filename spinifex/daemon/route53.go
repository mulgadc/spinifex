package daemon

import (
	"context"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/dns"
	reconcilelock "github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
	route53domain "github.com/mulgadc/spinifex/spinifex/domains/route53"
	"github.com/nats-io/nats.go/jetstream"
)

var _ route53domain.Publisher = (*dns.ZonePublisher)(nil)

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

// startRoute53Realizer publishes ACTIVE hosted zones to northstar. Every node
// runs it, and a per-pass election picks the one that writes. Without a zone
// store the zones stay unpublished and their changes stay PENDING.
func (d *Daemon) startRoute53Realizer() {
	pub, ok := dns.NewZonePublisher(d.config, func() *config.ClusterConfig { return d.clusterConfig }, d.natsConn)
	if !ok {
		slog.Info("Route 53 realizer not started: no northstar zone store is configured")
		return
	}
	r := route53domain.NewRealizer(d.route53Service.Store(), pub)
	r.Elect = func(ctx context.Context) (func(), bool) {
		return reconcilelock.AcquireLeader(ctx, d.natsConn, route53domain.KVBucketRoute53Realize, d.node)
	}
	r.Reserved = d.route53Service.Reserved
	go r.Run(d.ctx)
	slog.Info("Started Route 53 realizer")
}
