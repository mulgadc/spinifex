package daemon

import (
	"context"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/dns"
	reconcilelock "github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
	route53domain "github.com/mulgadc/spinifex/spinifex/domains/route53"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
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
		var services string
		if d.clusterConfig != nil {
			services = d.clusterConfig.AWS.ServicesDomain
		}
		return dns.ReservedZoneName(name, dns.ResolveBaseDomain(d.config), dns.ResolveInternalDomain(d.config), services)
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

// newRoute53Verifier checks each zone's delegation: from this cluster's own
// records when the parent is a zone it serves, and from the parent's public
// servers otherwise.
func (d *Daemon) newRoute53Verifier() *route53domain.Verifier {
	v := route53domain.NewVerifier(d.route53Service.Store())
	v.Nameservers = d.route53Service.Nameservers
	if d.dnsWriter.Enabled() {
		v.Platform = func(_ context.Context, name string) (route53domain.Delegation, bool, error) {
			servers, zone, ok, err := d.dnsWriter.PlatformDelegation(name)
			return route53domain.Delegation{Source: "platform zone " + zone, Servers: servers}, ok, err
		}
	}
	lookup := &dns.ParentLookup{}
	v.Public = func(ctx context.Context, name string) (route53domain.Delegation, error) {
		servers, parent, err := lookup.Delegation(ctx, name)
		return route53domain.Delegation{Source: "the " + parent + " name servers", Servers: servers}, err
	}
	v.Elect = func(ctx context.Context) (func(), bool) {
		return reconcilelock.AcquireLeader(ctx, d.natsConn, route53domain.KVBucketRoute53Verify, d.node)
	}
	return v
}

// route53Delegate writes or withdraws platform-zone delegations for the
// operator's spx admin dns delegate.
func (d *Daemon) route53Delegate(ctx context.Context, in *route53domain.AdminDelegateInput, _ string) (*route53domain.AdminDelegateOutput, error) {
	if len(in.Names) == 0 {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "no names to delegate")
	}
	res, err := d.dnsWriter.Delegate(ctx, d.natsConn, d.node, in.Names, in.Remove)
	if err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidInput, "%v", err)
	}
	return &route53domain.AdminDelegateOutput{Applied: res.Applied}, nil
}
