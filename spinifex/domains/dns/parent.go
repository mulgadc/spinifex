package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	mdns "github.com/miekg/dns"
)

// RootServers are the IANA root servers, by IPv4 address.
var RootServers = []string{
	"198.41.0.4", "170.247.170.2", "192.33.4.12", "199.7.91.13", "192.203.230.10",
	"192.5.5.241", "192.112.36.4", "198.97.190.53", "192.36.148.17", "192.58.128.30",
	"193.0.14.129", "199.7.83.42", "202.12.27.33",
}

const (
	parentQueryTimeout = 3 * time.Second
	maxReferrals       = 16
)

// ParentLookup reads a domain's delegation from its parent zone's own servers,
// found by following referrals down from the roots. It never asks a recursive
// resolver, whose cache can hold a delegation the parent has since withdrawn.
type ParentLookup struct {
	// Roots are the servers the walk starts from; nil means RootServers.
	Roots []string
	// Exchange sends one query; nil means UDP to port 53, retried over TCP
	// when truncated.
	Exchange func(ctx context.Context, m *mdns.Msg, server string) (*mdns.Msg, error)
	// Resolve finds a name server's addresses when a referral has no glue.
	Resolve func(ctx context.Context, host string) ([]string, error)
}

// Delegation returns the NS names the parent of name publishes for it and the
// parent zone that answered. An empty set with a nil error means the parent
// answered and does not delegate name.
func (p *ParentLookup) Delegation(ctx context.Context, name string) (servers []string, parent string, err error) {
	target := mdns.Fqdn(strings.ToLower(name))
	addrs := p.Roots
	if addrs == nil {
		addrs = RootServers
	}
	zone := "."
	for range maxReferrals {
		resp, err := p.ask(ctx, target, addrs)
		if err != nil {
			return nil, zone, fmt.Errorf("ask the %s servers: %w", zone, err)
		}
		owner, ns := referral(resp, target)
		if owner == "" || resp.Authoritative {
			// The servers hold target's own zone or its parent: what they list
			// for target is the answer, and nothing listed is no delegation.
			return answerNS(resp, target), strings.TrimSuffix(zone, "."), nil
		}
		if owner == target {
			return ns, strings.TrimSuffix(zone, "."), nil
		}
		if !mdns.IsSubDomain(zone, owner) || owner == zone {
			return nil, zone, fmt.Errorf("the %s servers referred %s back to %s", zone, target, owner)
		}
		zone = owner
		if addrs, err = p.addresses(ctx, resp, ns); err != nil {
			return nil, zone, err
		}
	}
	return nil, zone, fmt.Errorf("more than %d referrals for %s", maxReferrals, target)
}

// ask sends the query to each server in turn and returns the first answer
// that is NOERROR or NXDOMAIN.
func (p *ParentLookup) ask(ctx context.Context, target string, servers []string) (*mdns.Msg, error) {
	m := new(mdns.Msg)
	m.SetQuestion(target, mdns.TypeNS)
	m.RecursionDesired = false
	exchange := p.Exchange
	if exchange == nil {
		exchange = exchangeUDP
	}
	lastErr := errors.New("no servers")
	for _, s := range servers {
		resp, err := exchange(ctx, m, s)
		switch {
		case err != nil:
			lastErr = err
		case resp.Rcode != mdns.RcodeSuccess && resp.Rcode != mdns.RcodeNameError:
			lastErr = fmt.Errorf("%s answered %s", s, mdns.RcodeToString[resp.Rcode])
		default:
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// referral returns the deepest zone at or above target that the response's
// authority section delegates, and its NS names.
func referral(resp *mdns.Msg, target string) (string, []string) {
	var owner string
	var ns []string
	for _, rr := range resp.Ns {
		r, ok := rr.(*mdns.NS)
		if !ok {
			continue
		}
		o := strings.ToLower(r.Hdr.Name)
		if !mdns.IsSubDomain(o, target) {
			continue
		}
		if o != owner && (owner == "" || mdns.CountLabel(o) > mdns.CountLabel(owner)) {
			owner, ns = o, nil
		}
		if o == owner {
			ns = append(ns, strings.ToLower(r.Ns))
		}
	}
	return owner, ns
}

func answerNS(resp *mdns.Msg, target string) []string {
	var ns []string
	for _, rr := range append(resp.Answer, resp.Ns...) {
		if r, ok := rr.(*mdns.NS); ok && strings.EqualFold(r.Hdr.Name, target) {
			ns = append(ns, strings.ToLower(r.Ns))
		}
	}
	return ns
}

// addresses returns where to send the next query: the referral's glue, or a
// lookup of the server names when the parent gave none.
func (p *ParentLookup) addresses(ctx context.Context, resp *mdns.Msg, ns []string) ([]string, error) {
	var addrs []string
	for _, rr := range resp.Extra {
		if a, ok := rr.(*mdns.A); ok && slices.ContainsFunc(ns, func(n string) bool { return strings.EqualFold(n, a.Hdr.Name) }) {
			addrs = append(addrs, a.A.String())
		}
	}
	if len(addrs) > 0 {
		return addrs, nil
	}
	resolve := p.Resolve
	if resolve == nil {
		resolve = net.DefaultResolver.LookupHost
	}
	var lastErr error
	for _, host := range ns {
		found, err := resolve(ctx, strings.TrimSuffix(host, "."))
		if err != nil {
			lastErr = err
			continue
		}
		for _, a := range found {
			if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
				addrs = append(addrs, a)
			}
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no address for any of %s: %w", strings.Join(ns, ", "), lastErr)
	}
	return addrs, nil
}

func exchangeUDP(ctx context.Context, m *mdns.Msg, server string) (*mdns.Msg, error) {
	addr := net.JoinHostPort(server, "53")
	resp, _, err := (&mdns.Client{Timeout: parentQueryTimeout}).ExchangeContext(ctx, m, addr)
	if err == nil && resp.Truncated {
		resp, _, err = (&mdns.Client{Net: "tcp", Timeout: parentQueryTimeout}).ExchangeContext(ctx, m, addr)
	}
	return resp, err
}
