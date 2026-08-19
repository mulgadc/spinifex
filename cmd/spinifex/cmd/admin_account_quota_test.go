//test:in-package — drives the unexported flag table, parser and printer the quota subcommands are built from.
package cmd

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/gateway"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// newQuotaSetCmd builds a command carrying the same flags as the real one, so
// the parse is tested without mutating the package-level command between tests.
func newQuotaSetCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "set"}
	for _, d := range quotaDimensions {
		cmd.Flags().Int(d.flag, 0, d.usage)
	}
	cmd.Flags().Bool("clear", false, "")
	return cmd
}

// Only the flags actually passed become overrides. Every other dimension must
// stay nil so it keeps inheriting, which is the whole point of a sparse record.
func TestQuotaOverridesFromFlagsIsSparse(t *testing.T) {
	cmd := newQuotaSetCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--vcpus", "32", "--rds-instances", "8"}))

	over, changed, err := quotaOverridesFromFlags(cmd)
	require.NoError(t, err)
	require.True(t, changed)

	require.NotNil(t, over.VCPUs)
	require.Equal(t, 32, *over.VCPUs)
	require.NotNil(t, over.RDSInstances)
	require.Equal(t, 8, *over.RDSInstances)

	require.Nil(t, over.VPCs)
	require.Nil(t, over.EIPs)
	require.Nil(t, over.Volumes)
	require.Nil(t, over.VolumesGiB)
	require.Nil(t, over.Subnets)
	require.Nil(t, over.LoadBalancers)
}

// A flag left unset must not be read as an explicit 0, which would deny every
// request on that dimension instead of inheriting it.
func TestQuotaOverridesFromFlagsUnsetIsNotZero(t *testing.T) {
	cmd := newQuotaSetCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--vpcs", "8"}))

	over, changed, err := quotaOverridesFromFlags(cmd)
	require.NoError(t, err)
	require.True(t, changed)
	require.Nil(t, over.VCPUs, "an unset flag must not become an explicit zero")
}

// Passing 0 deliberately is a real limit and must be preserved.
func TestQuotaOverridesFromFlagsExplicitZero(t *testing.T) {
	cmd := newQuotaSetCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--eips", "0"}))

	over, changed, err := quotaOverridesFromFlags(cmd)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, over.EIPs)
	require.Equal(t, 0, *over.EIPs)
}

func TestQuotaOverridesFromFlagsUnlimited(t *testing.T) {
	cmd := newQuotaSetCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--vcpus", "-1"}))

	over, _, err := quotaOverridesFromFlags(cmd)
	require.NoError(t, err)
	require.NotNil(t, over.VCPUs)
	require.Equal(t, handlers_quota.Unlimited, *over.VCPUs)
}

// Anything below the Unlimited sentinel is a typo, not a smaller limit.
func TestQuotaOverridesFromFlagsRejectsBelowUnlimited(t *testing.T) {
	cmd := newQuotaSetCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--vcpus", "-2"}))

	_, _, err := quotaOverridesFromFlags(cmd)
	require.Error(t, err)
}

// No flags at all means nothing changed, which the caller turns into an error
// rather than a write that silently does nothing.
func TestQuotaOverridesFromFlagsNoneChanged(t *testing.T) {
	cmd := newQuotaSetCmd()
	require.NoError(t, cmd.ParseFlags(nil))

	over, changed, err := quotaOverridesFromFlags(cmd)
	require.NoError(t, err)
	require.False(t, changed)
	require.True(t, over.Empty())
}

// The table must name every dimension, label its source, and render the
// Unlimited sentinel as a word rather than as -1, which reads like a bug.
func TestPrintAccountQuota(t *testing.T) {
	resp := &gateway.AccountQuotaResponse{
		AccountID: "000000000042",
		Limits:    map[string]int{"vcpus": 32, "vpcs": handlers_quota.Unlimited, "eips": 4},
		Source:    map[string]string{"vcpus": "override", "vpcs": "override", "eips": "config"},
		Overrides: handlers_quota.Overrides{UpdatedBy: "operator", UpdatedAt: "2026-08-20T01:00:00Z"},
	}

	out := captureStdout(t, func() { printAccountQuota(resp) })

	require.Contains(t, out, "000000000042")
	require.Contains(t, out, "vcpus")
	require.Contains(t, out, "32")
	require.Contains(t, out, "override")
	require.Contains(t, out, "config")
	require.Contains(t, out, "unlimited")
	require.NotContains(t, out, "-1")
	require.Contains(t, out, "operator")
}

// Every dimension the override record carries must be reachable from the CLI,
// or a limit exists that an operator cannot set.
func TestQuotaDimensionsCoverEveryOverrideField(t *testing.T) {
	var over handlers_quota.Overrides
	for _, d := range quotaDimensions {
		v := 1
		*d.field(&over) = &v
	}
	require.False(t, over.Empty())

	seen := map[string]bool{}
	for _, d := range quotaDimensions {
		require.False(t, seen[d.flag], "duplicate flag %q", d.flag)
		seen[d.flag] = true
	}
	require.Len(t, quotaDimensions, 8)
}
