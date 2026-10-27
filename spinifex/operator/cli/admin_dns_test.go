package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	dnsdomain "github.com/mulgadc/spinifex/spinifex/domains/dns"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompareDelegation(t *testing.T) {
	published := []dnsdomain.Nameserver{
		{Host: "ns1", Node: "node1", IP: "192.0.2.1"},
		{Host: "ns2", Node: "node2", IP: "192.0.2.2"},
		{Host: "ns3", Node: "node3", IP: "192.0.2.3"},
		{Host: "ns4", Node: "node4", IP: "192.0.2.4"},
	}
	// The parent's answer: names are FQDNs in whatever case the registry
	// stores, ns3's glue is stale, ns4 is missing and ns9 is a leftover.
	listed := []string{"NS1.spx3.net.", "ns2.spx3.net.", "ns3.spx3.net.", "ns9.spx3.net."}
	glue := map[string][]string{
		"NS1.spx3.net.": {"192.0.2.1"},
		"ns3.spx3.net.": {"198.51.100.3"},
		"ns9.spx3.net.": {"198.51.100.9"},
	}

	got := map[string]delegationStatus{}
	for _, r := range compareDelegation("spx3.net", published, listed, glue) {
		got[r.Name] = r.Status
	}
	assert.Equal(t, map[string]delegationStatus{
		"ns1.spx3.net": delegationOK,
		"ns2.spx3.net": delegationNoGlue,
		"ns3.spx3.net": delegationBadGlue,
		"ns4.spx3.net": delegationMissing,
		"ns9.spx3.net": delegationNotOurs,
	}, got)
}

func dnsClusterToml(t *testing.T, nodes int, extra string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("node = \"node1\"\n\n" + extra + "\n")
	for i := 1; i <= nodes; i++ {
		fmt.Fprintf(&b, "[nodes.node%d]\nregion = \"us-east-1\"\nadvertise = \"192.0.2.%d\"\n\n[nodes.node%d.northstar]\nconfig_path = \"/etc/spinifex/northstar/northstar.toml\"\n", i, i, i)
		if i == 1 {
			b.WriteString("default_domain = \"spx3.net\"\n")
		}
		b.WriteString("\n")
	}
	return writeSpinifexToml(t, b.String())
}

func runNameservers(t *testing.T, path string) (string, error) {
	t.Helper()
	resetGlobalViper(t)
	// LoadConfig declares the process-wide cluster size; undo it so later
	// tests in this package can declare their own.
	clustersize.ResetForTest()
	t.Cleanup(clustersize.ResetForTest)
	viper.Set("config", path)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.Flags().Bool("check", false, "")
	cmd.Flags().Duration("timeout", time.Second, "")
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	err := runDNSNameservers(cmd, nil)
	return out.String(), err
}

func TestRunDNSNameservers_PublishesAtMostFour(t *testing.T) {
	out, err := runNameservers(t, dnsClusterToml(t, 5, ""))
	require.NoError(t, err)
	for i := 1; i <= 4; i++ {
		assert.Contains(t, out, fmt.Sprintf("ns%d.spx3.net  node%d  192.0.2.%d", i, i, i))
	}
	assert.NotContains(t, out, "ns5")
	assert.NotContains(t, out, "node5")
}

func TestRunDNSNameservers_FollowsPin(t *testing.T) {
	out, err := runNameservers(t, dnsClusterToml(t, 3, "[northstar]\nnameserver_count = 2\nnameserver_nodes = [\"node3\"]\n"))
	require.NoError(t, err)
	assert.Contains(t, out, "ns1.spx3.net  node3  192.0.2.3")
	assert.Contains(t, out, "ns2.spx3.net  node1  192.0.2.1")
	assert.NotContains(t, out, "ns3")
}

func TestRunDNSNameservers_RequiresBaseDomain(t *testing.T) {
	path := writeSpinifexToml(t, "node = \"node1\"\n\n[nodes.node1]\nregion = \"us-east-1\"\n")
	_, err := runNameservers(t, path)
	require.ErrorContains(t, err, "base domain")
}

// startDNS serves handler on a loopback UDP port and returns its address.
func startDNS(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &dns.Server{PacketConn: pc, Handler: handler}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

// A parent server that refuses is skipped; the first referral is read for the
// listed names and their glue, and recursion is never asked for.
func TestExchangeDelegation(t *testing.T) {
	refused := startDNS(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeRefused)
		_ = w.WriteMsg(m)
	})
	var sawRecursion atomic.Bool
	referral := startDNS(t, func(w dns.ResponseWriter, r *dns.Msg) {
		if r.RecursionDesired {
			sawRecursion.Store(true)
		}
		m := new(dns.Msg)
		m.SetReply(r)
		for _, rr := range []string{
			"spx3.net. 172800 IN NS ns1.spx3.net.",
			"spx3.net. 172800 IN NS ns2.spx3.net.",
			"other.net. 172800 IN NS ns.other.net.",
		} {
			m.Ns = append(m.Ns, mustRR(t, rr))
		}
		m.Extra = append(m.Extra, mustRR(t, "ns1.spx3.net. 172800 IN A 192.0.2.1"), mustRR(t, "ns2.spx3.net. 172800 IN AAAA 2001:db8::2"))
		_ = w.WriteMsg(m)
	})

	listed, glue, server, err := exchangeDelegation(context.Background(), "spx3.net", []string{refused, referral}, time.Second)
	require.NoError(t, err)
	assert.False(t, sawRecursion.Load(), "the parent must be asked without recursion")
	assert.Equal(t, referral, server)
	assert.Equal(t, []string{"ns1.spx3.net.", "ns2.spx3.net."}, listed)
	assert.Equal(t, map[string][]string{"ns1.spx3.net.": {"192.0.2.1"}, "ns2.spx3.net.": {"2001:db8::2"}}, glue)

	_, _, _, err = exchangeDelegation(context.Background(), "spx3.net", []string{refused}, time.Second)
	require.ErrorContains(t, err, "REFUSED")
}

func mustRR(t *testing.T, s string) dns.RR {
	t.Helper()
	rr, err := dns.NewRR(s)
	require.NoError(t, err)
	return rr
}

func TestPrintDelegation(t *testing.T) {
	var out bytes.Buffer
	printDelegation(&out, []delegationRow{
		{Name: "ns1.spx3.net", Want: "192.0.2.1", Glue: []string{"192.0.2.1"}, Status: delegationOK},
		{Name: "ns9.spx3.net", Status: delegationNotOurs},
	})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, []string{"ns1.spx3.net", "192.0.2.1", "192.0.2.1", "ok"}, strings.Fields(lines[1]))
	assert.Equal(t, []string{"ns9.spx3.net", "-", "-", "not", "published", "here"}, strings.Fields(lines[2]))
}
