package cmd

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/mulgadc/spinifex/spinifex/gateway"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	"github.com/spf13/cobra"
)

// quotaDimensions maps each CLI flag to the override field it sets. One table
// drives the flag registration and the parse, so a new dimension cannot be
// added to one and forgotten in the other.
var quotaDimensions = []struct {
	flag  string
	usage string
	field func(*handlers_quota.Overrides) **int
}{
	{"vcpus", "Total vCPUs across running and stopped instances", func(o *handlers_quota.Overrides) **int { return &o.VCPUs }},
	{"vpcs", "VPCs", func(o *handlers_quota.Overrides) **int { return &o.VPCs }},
	{"subnets", "Subnets, counted per account rather than per VPC", func(o *handlers_quota.Overrides) **int { return &o.Subnets }},
	{"eips", "Elastic IP addresses", func(o *handlers_quota.Overrides) **int { return &o.EIPs }},
	{"volumes", "EBS volumes", func(o *handlers_quota.Overrides) **int { return &o.Volumes }},
	{"volumes-gib", "Total EBS capacity in GiB", func(o *handlers_quota.Overrides) **int { return &o.VolumesGiB }},
	{"rds-instances", "RDS database instances", func(o *handlers_quota.Overrides) **int { return &o.RDSInstances }},
	{"load-balancers", "Load balancers, ALB and NLB together", func(o *handlers_quota.Overrides) **int { return &o.LoadBalancers }},
}

var accountQuotaCmd = &cobra.Command{
	Use:   "quota",
	Short: "Inspect and adjust an account's service quotas",
	Long: `Inspect and adjust the service quotas applied to one account.

Every account inherits the [quota] limits from awsgw.toml. An override raises or
lowers individual dimensions for one account without affecting any other, so a
customer can be given more vCPUs while every other tenant stays on the baseline.

Only the dimensions passed as flags are changed; the rest keep inheriting. Use
-1 for no limit on a dimension, and --clear to drop every override.`,
}

var accountQuotaGetCmd = &cobra.Command{
	Use:   "get <account-id>",
	Short: "Show the limits in force for an account",
	Long: `Show the limits in force for an account and where each came from:
"config" for an inherited baseline, "override" for one set on this account.`,
	Args: cobra.ExactArgs(1),
	RunE: runAccountQuotaGet,
}

var accountQuotaSetCmd = &cobra.Command{
	Use:   "set <account-id>",
	Short: "Override one or more limits for an account",
	Long: `Override one or more limits for an account. Dimensions not named keep
inheriting the configured baseline.

  spx admin account quota set 000000000005 --vcpus 32
  spx admin account quota set 000000000005 --clear`,
	Args: cobra.ExactArgs(1),
	RunE: runAccountQuotaSet,
}

// Both subcommands are served only by the /admin surface, so unlike the account
// commands there is no NATS path and no --remote to opt into.
func init() {
	accountCmd.AddCommand(accountQuotaCmd)
	accountQuotaCmd.AddCommand(accountQuotaGetCmd)
	accountQuotaCmd.AddCommand(accountQuotaSetCmd)

	for _, c := range []*cobra.Command{accountQuotaGetCmd, accountQuotaSetCmd} {
		c.Flags().String("endpoint", "", "Gateway endpoint (default: this node's AWS gateway)")
		c.Flags().String("region", "", "SigV4 region (default: this node's region)")
		c.Flags().String("ca-bundle", "", "CA certificate (default: this node's CA)")
	}

	for _, d := range quotaDimensions {
		accountQuotaSetCmd.Flags().Int(d.flag, 0, d.usage+" (-1 for no limit)")
	}
	accountQuotaSetCmd.Flags().Bool("clear", false, "Remove every override, returning the account to the configured limits")
}

func runAccountQuotaGet(cmd *cobra.Command, args []string) error {
	target, err := resolveAdminTarget(cmd)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), adminRequestTimeout)
	defer cancel()

	var out gateway.AccountQuotaResponse
	req := gateway.AccountQuotaRequest{AccountID: args[0]}
	if err := callAdmin(ctx, target, "GetAccountQuota", req, &out); err != nil {
		return err
	}
	printAccountQuota(&out)
	return nil
}

func runAccountQuotaSet(cmd *cobra.Command, args []string) error {
	target, err := resolveAdminTarget(cmd)
	if err != nil {
		return err
	}

	over, changed, err := quotaOverridesFromFlags(cmd)
	if err != nil {
		return err
	}
	clearAll, err := cmd.Flags().GetBool("clear")
	if err != nil {
		return err
	}
	// Refusing both is not pedantry: --clear silently winning would discard
	// limits the caller believed it was setting in the same breath.
	if clearAll && changed {
		return fmt.Errorf("--clear cannot be combined with a dimension flag")
	}
	if !clearAll && !changed {
		return fmt.Errorf("no dimension given; pass at least one limit flag, or --clear to remove every override")
	}
	if clearAll {
		over = handlers_quota.Overrides{}
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), adminRequestTimeout)
	defer cancel()

	var out gateway.AccountQuotaResponse
	req := gateway.AccountQuotaRequest{AccountID: args[0], Overrides: over}
	if err := callAdmin(ctx, target, "PutAccountQuota", req, &out); err != nil {
		return err
	}
	printAccountQuota(&out)
	return nil
}

// quotaOverridesFromFlags builds the override set from the flags the caller
// actually passed. An unset flag is left nil so the dimension keeps inheriting,
// which is why Changed is consulted rather than the flag's value.
func quotaOverridesFromFlags(cmd *cobra.Command) (handlers_quota.Overrides, bool, error) {
	var over handlers_quota.Overrides
	changed := false
	for _, d := range quotaDimensions {
		if !cmd.Flags().Changed(d.flag) {
			continue
		}
		value, err := cmd.Flags().GetInt(d.flag)
		if err != nil {
			return over, false, err
		}
		if value < handlers_quota.Unlimited {
			return over, false, fmt.Errorf("--%s must be 0 or greater, or %d for no limit", d.flag, handlers_quota.Unlimited)
		}
		*d.field(&over) = &value
		changed = true
	}
	return over, changed, nil
}

func printAccountQuota(resp *gateway.AccountQuotaResponse) {
	fmt.Printf("Account %s\n\n", resp.AccountID)
	fmt.Printf("%-16s %10s  %s\n", "DIMENSION", "LIMIT", "SOURCE")
	fmt.Printf("%-16s %10s  %s\n", "---------", "-----", "------")

	names := make([]string, 0, len(resp.Limits))
	for name := range resp.Limits {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		limit := strconv.Itoa(resp.Limits[name])
		if resp.Limits[name] == handlers_quota.Unlimited {
			limit = "unlimited"
		}
		fmt.Printf("%-16s %10s  %s\n", name, limit, resp.Source[name])
	}

	if resp.Overrides.UpdatedAt != "" {
		fmt.Printf("\nOverride set by %s at %s\n", resp.Overrides.UpdatedBy, resp.Overrides.UpdatedAt)
	}
}
