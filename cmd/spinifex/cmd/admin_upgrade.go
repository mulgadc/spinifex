package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/mulgadc/spinifex/spinifex/systemd"

	"github.com/spf13/cobra"
)

// systemdUnitRoot is where installed units live. A var, not a const, so
// tests could point it elsewhere if this command ever grows a test harness.
var systemdUnitRoot = "/etc/systemd/system"

// upgradeExit is os.Exit, overridable in tests so a failure path can be
// exercised without killing the test binary.
var upgradeExit = os.Exit

// errUpgradeFlagsConflict is returned when both --units-only and --skip-units
// are set, a combination that would leave nothing for the command to do.
var errUpgradeFlagsConflict = errors.New("--units-only and --skip-units are mutually exclusive")

// validateUpgradeFlags checks flag combinations that runAdminUpgrade cannot
// proceed with, kept separate from runAdminUpgrade so the check is testable
// without driving the whole command.
func validateUpgradeFlags(unitsOnly, skipUnits bool) error {
	if unitsOnly && skipUnits {
		return errUpgradeFlagsConflict
	}
	return nil
}

// upgradeSummary decides whether runAdminUpgrade has anything left to do
// after gathering config and unit status, and what to print when it does
// not. Kept pure so the dryRun × hasConfigWork × hasUnitWork matrix is
// testable without driving the whole command.
func upgradeSummary(dryRun, hasConfigWork, hasUnitWork bool) (proceed bool, message string) {
	if !hasConfigWork && !hasUnitWork {
		if dryRun {
			return false, "\nNothing pending — config and units are current."
		}
		return false, "\nNothing to do."
	}
	if dryRun {
		return false, ""
	}
	return true, ""
}

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Apply pending config migrations and systemd unit updates",
	Long: `Apply pending configuration file migrations and systemd unit reconciliation
for upgrades between Spinifex versions.

Swapping the spx binary alone does not upgrade a node: unit files (KillMode,
TimeoutStopSec, drain ordering, ...) are installed once at setup time and
never re-asserted otherwise. This command brings both config and units up to
date with the binary running it.

When run interactively (without --yes), shows what is pending and prompts
for confirmation. When called from setup.sh with --yes, applies immediately.
--dry-run reports drift without prompting or changing anything.

Operators can skip migrations during install by setting INSTALL_SPINIFEX_SKIP_MIGRATE=1,
then run 'spx admin upgrade' manually to review and apply.`,
	Run: runAdminUpgrade,
}

func runAdminUpgrade(cmd *cobra.Command, _ []string) {
	configDir, _ := cmd.Root().Flags().GetString("config-dir")
	dataDir, _ := cmd.Root().Flags().GetString("spinifex-dir")
	yes, _ := cmd.Flags().GetBool("yes")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	unitsOnly, _ := cmd.Flags().GetBool("units-only")
	skipUnits, _ := cmd.Flags().GetBool("skip-units")

	if err := validateUpgradeFlags(unitsOnly, skipUnits); err != nil {
		fmt.Fprintln(os.Stderr, err)
		upgradeExit(1)
	}

	// Check that the installation exists.
	spinifexToml := configDir + "/spinifex.toml"
	if _, err := os.Stat(spinifexToml); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "No Spinifex installation found at %s\nRun 'spx admin init' first.\n", configDir)
		upgradeExit(1)
	}

	var pending []migrate.PendingMigration
	if !unitsOnly {
		pending = reportConfigStatus(configDir)
	}

	var unitResult systemd.Result
	if !skipUnits {
		var err error
		unitResult, err = systemd.Reconcile(systemdUnitRoot, systemd.Options{DryRun: true})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error checking systemd units: %v\n", err)
			upgradeExit(1)
		}
		reportUnitStatus(unitResult)
	}

	hasConfigWork := !unitsOnly && len(pending) > 0
	hasUnitWork := !skipUnits && unitResult.HasChanges()

	proceed, message := upgradeSummary(dryRun, hasConfigWork, hasUnitWork)
	if message != "" {
		fmt.Println(message)
	}
	if !proceed {
		return
	}

	if !yes {
		fmt.Print("\nApply? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Aborted.")
			return
		}
	}

	if hasConfigWork {
		if err := migrate.DefaultRegistry.RunAllConfig(configDir, dataDir); err != nil {
			fmt.Fprintf(os.Stderr, "Config migration failed: %v\n", err)
			upgradeExit(1)
		}
	}

	if !skipUnits {
		applied, err := systemd.Reconcile(systemdUnitRoot, systemd.Options{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nUnit reconciliation failed: %v\n", err)
			if errors.Is(err, systemd.ErrRootRequired) {
				fmt.Fprintln(os.Stderr, "Re-run as root to apply the unit changes reported above: sudo spx admin upgrade --units-only --yes")
			}
			upgradeExit(1)
		}
		reportUnitApply(applied)
	}

	fmt.Println("\nDone. daemon-reload has already picked up any unit changes for the")
	fmt.Println("next stop; restart to apply config changes: sudo systemctl restart spinifex.target")
}

// reportConfigStatus prints current config versions and pending migrations,
// mirroring the pre-existing `spx admin upgrade` output.
func reportConfigStatus(configDir string) []migrate.PendingMigration {
	versions, err := migrate.DefaultRegistry.ConfigVersions(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading config versions: %v\n", err)
		upgradeExit(1)
	}

	fmt.Println("Reading current config versions...")
	for name, v := range versions {
		if v == 0 {
			fmt.Printf("  %-20s %d (no version marker)\n", name+":", v)
		} else {
			fmt.Printf("  %-20s %d\n", name+":", v)
		}
	}

	pending, err := migrate.DefaultRegistry.PendingConfig(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error checking pending migrations: %v\n", err)
		upgradeExit(1)
	}

	if len(pending) == 0 {
		fmt.Println("No pending config migrations.")
		return pending
	}

	fmt.Println("Pending config migrations:")
	for _, p := range pending {
		fmt.Printf("  [config] %s %d → %d: %s\n", p.Target, p.FromVersion, p.ToVersion, p.Description)
	}
	return pending
}

// reportUnitStatus prints the reconcile decision for each unit, computed
// with Options{DryRun: true} so this is always safe to call before a prompt.
func reportUnitStatus(result systemd.Result) {
	fmt.Println("\nReading installed systemd unit versions...")
	for _, s := range result.Statuses {
		switch s.Action {
		case systemd.ActionNoop:
			fmt.Printf("  %-38s %d (up to date)\n", s.Name+":", s.InstalledVersion)
		case systemd.ActionInstall:
			fmt.Printf("  %-38s missing → will install (v%d)\n", s.Name+":", s.EmbeddedVersion)
		case systemd.ActionReplace:
			fmt.Printf("  %-38s %d → %d (stale, will replace)\n", s.Name+":", s.InstalledVersion, s.EmbeddedVersion)
		case systemd.ActionConflict:
			fmt.Printf("  %-38s %d (operator-modified, not touched — see `systemctl edit %s`)\n", s.Name+":", s.InstalledVersion, s.Name)
		}
	}
	if result.HasConflicts() {
		fmt.Println("Operator-modified units above are left as-is; review them by hand.")
	}
}

// reportUnitApply prints backups written and confirms daemon-reload ran.
func reportUnitApply(result systemd.Result) {
	for _, s := range result.Statuses {
		if !s.Applied {
			continue
		}
		if s.Backup != "" {
			fmt.Printf("  [unit] %s replaced (backup: %s)\n", s.Name, s.Backup)
		} else {
			fmt.Printf("  [unit] %s installed\n", s.Name)
		}
	}
	if result.ReloadRan {
		fmt.Println("  systemctl daemon-reload complete.")
	}
}
