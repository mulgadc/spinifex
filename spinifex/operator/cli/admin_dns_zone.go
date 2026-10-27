package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	route53domain "github.com/mulgadc/spinifex/spinifex/domains/route53"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/spf13/cobra"
)

// route53AdminTimeout covers a delegation check, which walks the public DNS
// tree, and a delegate batch, which waits for the zone-writer election.
const route53AdminTimeout = 60 * time.Second

var dnsZoneCmd = &cobra.Command{
	Use:   "zone",
	Short: "Inspect and override the activation of customer hosted zones",
}

var dnsZoneStatusCmd = &cobra.Command{
	Use:   "status <zone-id|name>",
	Short: "Show a hosted zone's activation, in whichever account holds it",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		q := route53domain.AdminZoneQuery{}
		if looksLikeZoneID(args[0]) {
			q.ZoneID = args[0]
		} else {
			q.Name = args[0]
		}
		return runRoute53Admin(cmd, "route53.admin.FindZones", q)
	},
}

var dnsZoneVerifyCmd = &cobra.Command{
	Use:   "verify <zone-id>",
	Short: "Check a hosted zone's delegation now",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRoute53Admin(cmd, "route53.admin.Verify", route53domain.AdminZoneQuery{ZoneID: args[0]})
	},
}

var dnsZoneActivateCmd = &cobra.Command{
	Use:   "activate <zone-id> --reason <why>",
	Short: "Serve a hosted zone whatever its delegation, for private roots and air-gapped sites",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runZoneOverride(cmd, args[0], route53domain.StateActive)
	},
}

var dnsZoneDeactivateCmd = &cobra.Command{
	Use:   "deactivate <zone-id> --reason <why>",
	Short: "Stop serving a hosted zone, for a contested domain",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runZoneOverride(cmd, args[0], route53domain.StateSuspended)
	},
}

var dnsZoneReleaseCmd = &cobra.Command{
	Use:   "release <zone-id>",
	Short: "Clear an override and hand the zone back to the delegation check",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runZoneOverride(cmd, args[0], "")
	},
}

var dnsDelegateCmd = &cobra.Command{
	Use:   "delegate [name...]",
	Short: "Delegate names below the base domain to this cluster's nameservers",
	Long: `Write an NS set in the base zone delegating each name to the published nsN
nameservers, so a customer hosted zone of that name passes its delegation
check without any registrar. --remove withdraws the delegations; --file reads
one name per line, for batches.`,
	RunE: runDNSDelegate,
}

func init() {
	dnsCmd.AddCommand(dnsZoneCmd, dnsDelegateCmd)
	dnsZoneCmd.AddCommand(dnsZoneStatusCmd, dnsZoneVerifyCmd, dnsZoneActivateCmd, dnsZoneDeactivateCmd, dnsZoneReleaseCmd)
	for _, c := range []*cobra.Command{dnsZoneActivateCmd, dnsZoneDeactivateCmd} {
		c.Flags().String("reason", "", "Why, recorded on the zone (required)")
	}
	dnsDelegateCmd.Flags().Bool("remove", false, "Withdraw the delegations instead of writing them")
	dnsDelegateCmd.Flags().String("file", "", "Read names from this file, one per line ('-' for stdin)")
}

// route53AdminFn sends one admin request and returns the raw response; tests
// replace it.
var route53AdminFn = func(ctx context.Context, subject string, in any) (json.RawMessage, error) {
	_, nc, err := loadConfigAndConnectFn()
	if err != nil {
		return nil, err
	}
	defer nc.Close()
	out, err := natsmsg.NATSRequest[json.RawMessage](ctx, nc, subject, in, route53AdminTimeout, awsidentifiers.GlobalAccountID)
	if err != nil {
		return nil, err
	}
	return *out, nil
}

func runZoneOverride(cmd *cobra.Command, id, override string) error {
	reason := ""
	if f := cmd.Flags().Lookup("reason"); f != nil {
		reason = strings.TrimSpace(f.Value.String())
		if reason == "" {
			return errors.New("--reason is required")
		}
	}
	return runRoute53Admin(cmd, "route53.admin.Override", route53domain.AdminOverrideInput{ZoneID: id, Override: override, Reason: reason})
}

func runRoute53Admin(cmd *cobra.Command, subject string, in any) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), route53AdminTimeout)
	defer cancel()
	raw, err := route53AdminFn(ctx, subject, in)
	if err != nil {
		return err
	}
	var out route53domain.AdminZones
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(out.Zones) == 0 {
		return errors.New("no hosted zone matches")
	}
	printZones(cmd.OutOrStdout(), out.Zones)
	return nil
}

func printZones(out io.Writer, zones []route53domain.ZoneRecord) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ZONE ID\tACCOUNT\tNAME\tSTATE\tCHECKED\tPARENT\tDELEGATED TO\tREASON")
	for _, z := range zones {
		checked := "-"
		if !z.CheckedAt.IsZero() {
			checked = z.CheckedAt.UTC().Format(time.RFC3339)
		}
		state, reason := z.State, z.StateReason
		if z.Override != "" {
			state += " (override)"
			reason = z.OverrideReason
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", z.ID, z.AccountID, strings.TrimSuffix(z.Name, "."), state, checked,
			dashIfEmpty(z.DelegationSource), dashIfEmpty(strings.Join(z.Delegation, ",")), dashIfEmpty(reason))
	}
	_ = w.Flush()
}

func runDNSDelegate(cmd *cobra.Command, args []string) error {
	names := append([]string(nil), args...)
	if path, _ := cmd.Flags().GetString("file"); path != "" {
		more, err := readNames(cmd.InOrStdin(), path)
		if err != nil {
			return err
		}
		names = append(names, more...)
	}
	if len(names) == 0 {
		return errors.New("no names given")
	}
	remove, _ := cmd.Flags().GetBool("remove")
	ctx, cancel := context.WithTimeout(cmd.Context(), route53AdminTimeout)
	defer cancel()
	raw, err := route53AdminFn(ctx, "route53.admin.Delegate", route53domain.AdminDelegateInput{Names: names, Remove: remove})
	if err != nil {
		return err
	}
	var out route53domain.AdminDelegateOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	verb := "Delegated"
	if remove {
		verb = "Withdrew the delegation of"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %d name(s); %d record change(s) applied\n", verb, len(names), out.Applied)
	return nil
}

func readNames(stdin io.Reader, path string) ([]string, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	var names []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			names = append(names, line)
		}
	}
	return names, sc.Err()
}

// looksLikeZoneID tells a Route 53 zone ID from a domain name: an ID has no dot.
func looksLikeZoneID(s string) bool {
	s = strings.TrimPrefix(s, "/hostedzone/")
	return s != "" && !strings.Contains(s, ".")
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
