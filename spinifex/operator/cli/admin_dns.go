package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/miekg/dns"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	dnsdomain "github.com/mulgadc/spinifex/spinifex/domains/dns"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var dnsCmd = &cobra.Command{
	Use:   "dns",
	Short: "Inspect the cluster's authoritative DNS",
}

var dnsNameserversCmd = &cobra.Command{
	Use:   "nameservers",
	Short: "Print the published nsN nameservers and check their delegation",
	Long: `Print the nameservers this cluster publishes for its base domain: each nsN
name, the node it is assigned to and that node's address. At most four are
published however many nodes run northstar; [northstar] nameserver_count and
nameserver_nodes in spinifex.toml set the count and pin the order; a node
given --services northstar at init or join is pinned for you.

With --check, ask one of the parent zone's servers (for spx3.net, a .net
server) for the base domain's delegation without recursion, and report for
each nsN whether the parent lists it and whether its glue matches the node's
address. Exits non-zero if any does not.`,
	RunE: runDNSNameservers,
}

func init() {
	adminCmd.AddCommand(dnsCmd)
	dnsCmd.AddCommand(dnsNameserversCmd)
	dnsNameserversCmd.Flags().Bool("check", false, "Compare against the delegation published by the parent zone")
	dnsNameserversCmd.Flags().Duration("timeout", 5*time.Second, "Timeout for each DNS query made by --check")
}

func runDNSNameservers(cmd *cobra.Command, _ []string) error {
	cfgPath := viper.GetString("config")
	if cfgPath == "" {
		cfgPath = DefaultConfigFile()
	}
	cluster, err := config.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	local := cluster.Nodes[cluster.Node]
	base := strings.TrimSuffix(dnsdomain.ResolveBaseDomain(&local), ".")
	if base == "" {
		return errors.New("no northstar base domain is configured on this node")
	}
	published := dnsdomain.PublishedNameservers(cluster)
	if len(published) == 0 {
		return errors.New("no node qualifies as a nameserver: none runs northstar on a non-loopback address")
	}

	out := cmd.OutOrStdout()
	check, _ := cmd.Flags().GetBool("check")
	if !check {
		printNameservers(out, base, published)
		return nil
	}

	timeout, _ := cmd.Flags().GetDuration("timeout")
	ctx, cancel := context.WithTimeout(cmd.Context(), 4*timeout)
	defer cancel()
	listed, glue, server, err := queryParentDelegation(ctx, base, timeout)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Delegation of %s as published by %s\n\n", base, server)
	rows := compareDelegation(base, published, listed, glue)
	printDelegation(out, rows)
	for _, r := range rows {
		if r.Status != delegationOK {
			return errors.New("the parent zone's delegation does not match the published nameservers")
		}
	}
	return nil
}

func printNameservers(out io.Writer, base string, published []dnsdomain.Nameserver) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tNODE\tADDRESS")
	for _, ns := range published {
		fmt.Fprintf(w, "%s.%s\t%s\t%s\n", ns.Host, base, ns.Node, ns.IP)
	}
	_ = w.Flush()
}

type delegationStatus string

const (
	delegationOK      delegationStatus = "ok"
	delegationMissing delegationStatus = "not delegated"
	delegationNoGlue  delegationStatus = "no glue"
	delegationBadGlue delegationStatus = "glue mismatch"
	delegationNotOurs delegationStatus = "not published here"
)

type delegationRow struct {
	Name   string
	Want   string
	Glue   []string
	Status delegationStatus
}

// compareDelegation checks the parent's NS names and glue for base against the
// published nameservers. A name the parent lists that this cluster does not
// publish is reported too, since resolvers will send queries to it.
func compareDelegation(base string, published []dnsdomain.Nameserver, listed []string, glue map[string][]string) []delegationRow {
	norm := func(name string) string { return strings.ToLower(strings.TrimSuffix(name, ".")) }
	listedSet := make(map[string]bool, len(listed))
	for _, n := range listed {
		listedSet[norm(n)] = true
	}
	normGlue := make(map[string][]string, len(glue))
	for n, ips := range glue {
		normGlue[norm(n)] = ips
	}

	rows := make([]delegationRow, 0, len(published)+len(listed))
	ours := make(map[string]bool, len(published))
	for _, ns := range published {
		name := norm(ns.Host + "." + base)
		ours[name] = true
		row := delegationRow{Name: name, Want: ns.IP, Glue: normGlue[name]}
		switch {
		case !listedSet[name]:
			row.Status = delegationMissing
		case len(row.Glue) == 0:
			row.Status = delegationNoGlue
		case !slices.Contains(row.Glue, ns.IP):
			row.Status = delegationBadGlue
		default:
			row.Status = delegationOK
		}
		rows = append(rows, row)
	}
	for _, n := range listed {
		if name := norm(n); !ours[name] {
			rows = append(rows, delegationRow{Name: name, Glue: normGlue[name], Status: delegationNotOurs})
			ours[name] = true
		}
	}
	return rows
}

func printDelegation(out io.Writer, rows []delegationRow) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tWANT\tPARENT GLUE\tSTATUS")
	for _, r := range rows {
		want, glue := r.Want, strings.Join(r.Glue, ",")
		if want == "" {
			want = "-"
		}
		if glue == "" {
			glue = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, want, glue, r.Status)
	}
	_ = w.Flush()
}

// queryParentDelegation asks a server of base's parent zone, without
// recursion, for base's NS set.
func queryParentDelegation(ctx context.Context, base string, timeout time.Duration) ([]string, map[string][]string, string, error) {
	parent := base[strings.Index(base, ".")+1:]
	if parent == base {
		return nil, nil, "", fmt.Errorf("%s has no parent zone to check", base)
	}
	servers, err := net.DefaultResolver.LookupNS(ctx, parent)
	if err != nil || len(servers) == 0 {
		return nil, nil, "", fmt.Errorf("find the %s servers: %w", parent, err)
	}
	addrs := make([]string, 0, len(servers))
	for _, s := range servers {
		addrs = append(addrs, net.JoinHostPort(strings.TrimSuffix(s.Host, "."), "53"))
	}
	return exchangeDelegation(ctx, base, addrs, timeout)
}

// exchangeDelegation asks each server in turn for base's NS set and returns,
// from the first that answers, the names it lists and the glue addresses in
// its additional section.
func exchangeDelegation(ctx context.Context, base string, servers []string, timeout time.Duration) ([]string, map[string][]string, string, error) {
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(base), dns.TypeNS)
	msg.RecursionDesired = false
	client := &dns.Client{Timeout: timeout}

	lastErr := errors.New("no servers")
	for _, server := range servers {
		resp, _, err := client.ExchangeContext(ctx, msg, server)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.Rcode != dns.RcodeSuccess {
			lastErr = fmt.Errorf("%s answered %s", server, dns.RcodeToString[resp.Rcode])
			continue
		}
		var listed []string
		for _, rr := range append(resp.Ns, resp.Answer...) {
			if ns, ok := rr.(*dns.NS); ok && strings.EqualFold(ns.Hdr.Name, dns.Fqdn(base)) {
				listed = append(listed, ns.Ns)
			}
		}
		glue := map[string][]string{}
		for _, rr := range resp.Extra {
			switch a := rr.(type) {
			case *dns.A:
				glue[a.Hdr.Name] = append(glue[a.Hdr.Name], a.A.String())
			case *dns.AAAA:
				glue[a.Hdr.Name] = append(glue[a.Hdr.Name], a.AAAA.String())
			}
		}
		return listed, glue, server, nil
	}
	return nil, nil, "", fmt.Errorf("no server answered for %s: %w", base, lastErr)
}
