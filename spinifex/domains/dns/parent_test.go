package dns_test

import (
	"context"
	"errors"
	"testing"

	mdns "github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/domains/dns"
)

// fakeTree is the DNS hierarchy as each server's address answers it.
type fakeTree struct {
	servers map[string]func(q mdns.Question) *mdns.Msg
	asked   []string
}

func (f *fakeTree) exchange(_ context.Context, m *mdns.Msg, server string) (*mdns.Msg, error) {
	f.asked = append(f.asked, server)
	if m.RecursionDesired {
		return nil, errors.New("recursion requested")
	}
	h, ok := f.servers[server]
	if !ok {
		return nil, errors.New("i/o timeout")
	}
	resp := h(m.Question[0])
	rcode, aa := resp.Rcode, resp.Authoritative
	resp.SetReply(m)
	resp.Rcode, resp.Authoritative = rcode, aa
	return resp, nil
}

func ns(owner string, hosts ...string) []mdns.RR {
	var out []mdns.RR
	for _, h := range hosts {
		out = append(out, &mdns.NS{Hdr: mdns.RR_Header{Name: owner, Rrtype: mdns.TypeNS, Class: mdns.ClassINET, Ttl: 172800}, Ns: h})
	}
	return out
}

func glue(host, ip string) mdns.RR {
	rr, _ := mdns.NewRR(host + " 172800 IN A " + ip)
	return rr
}

func referralTo(owner string, extra []mdns.RR, hosts ...string) *mdns.Msg {
	return &mdns.Msg{Ns: ns(owner, hosts...), Extra: extra}
}

func authoritative(rcode int) *mdns.Msg {
	m := &mdns.Msg{}
	m.Authoritative = true
	m.Rcode = rcode
	return m
}

func newTree() *fakeTree {
	return &fakeTree{servers: map[string]func(mdns.Question) *mdns.Msg{
		"10.0.0.1": func(q mdns.Question) *mdns.Msg {
			if mdns.IsSubDomain("org.", q.Name) {
				return referralTo("org.", nil, "a0.org-servers.test.") // no glue
			}
			return referralTo("com.", []mdns.RR{glue("a.gtld.test.", "10.0.1.1")}, "a.gtld.test.")
		},
		"10.0.1.1": func(q mdns.Question) *mdns.Msg {
			switch {
			case mdns.IsSubDomain("example.com.", q.Name):
				return referralTo("example.com.", nil, "NS1.spx3.net.", "ns2.spx3.net.")
			case mdns.IsSubDomain("deep.com.", q.Name):
				return referralTo("deep.com.", []mdns.RR{glue("ns.deep.com.", "10.0.2.1")}, "ns.deep.com.")
			default:
				return authoritative(mdns.RcodeNameError)
			}
		},
		"10.0.2.1": func(q mdns.Question) *mdns.Msg {
			if q.Name == "shop.deep.com." {
				return referralTo("shop.deep.com.", nil, "ns1.spx3.net.")
			}
			return authoritative(mdns.RcodeSuccess) // NODATA: not delegated
		},
		"10.0.3.1": func(q mdns.Question) *mdns.Msg {
			return referralTo(q.Name, nil, "ns3.spx3.net.")
		},
	}}
}

func TestParentLookupFollowsReferralsToTheParent(t *testing.T) {
	tree := newTree()
	p := &dns.ParentLookup{
		Roots:    []string{"10.0.0.9", "10.0.0.1"}, // the first root is down
		Exchange: tree.exchange,
		Resolve: func(_ context.Context, host string) ([]string, error) {
			if host == "a0.org-servers.test" {
				return []string{"2001:db8::1", "10.0.3.1"}, nil
			}
			return nil, errors.New("no such host")
		},
	}
	cases := []struct {
		name    string
		servers []string
		parent  string
	}{
		{"Example.COM", []string{"ns1.spx3.net.", "ns2.spx3.net."}, "com"},
		{"shop.deep.com.", []string{"ns1.spx3.net."}, "deep.com"},
		{"other.deep.com", nil, "deep.com"},
		{"unregistered.com", nil, "com"},
		{"example.org", []string{"ns3.spx3.net."}, "org"},
	}
	for _, tc := range cases {
		servers, parent, err := p.Delegation(context.Background(), tc.name)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.servers, servers, tc.name)
		assert.Equal(t, tc.parent, parent, tc.name)
	}
}

func TestParentLookupFailsWhenNoServerAnswers(t *testing.T) {
	tree := newTree()
	tree.servers["10.0.1.1"] = func(mdns.Question) *mdns.Msg { return &mdns.Msg{MsgHdr: mdns.MsgHdr{Rcode: mdns.RcodeServerFailure}} }
	p := &dns.ParentLookup{Roots: []string{"10.0.0.1"}, Exchange: tree.exchange}
	_, parent, err := p.Delegation(context.Background(), "example.com")
	require.ErrorContains(t, err, "SERVFAIL")
	assert.Equal(t, "com.", parent)
}

func TestParentLookupRefusesAReferralUpwards(t *testing.T) {
	tree := newTree()
	tree.servers["10.0.1.1"] = func(mdns.Question) *mdns.Msg {
		return referralTo(".", []mdns.RR{glue("a.root.test.", "10.0.0.1")}, "a.root.test.")
	}
	p := &dns.ParentLookup{Roots: []string{"10.0.0.1"}, Exchange: tree.exchange}
	_, _, err := p.Delegation(context.Background(), "example.com")
	require.Error(t, err)
}
