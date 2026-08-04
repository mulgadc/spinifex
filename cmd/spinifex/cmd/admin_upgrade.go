package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/migrate"
	"github.com/spf13/cobra"
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Apply pending config migrations",
	Long: `Apply pending configuration file migrations for upgrades between Spinifex versions.

When run interactively (without --yes), shows pending migrations and prompts
for confirmation. When called from setup.sh with --yes, applies immediately.

Operators can skip migrations during install by setting INSTALL_SPINIFEX_SKIP_MIGRATE=1,
then run 'spx admin upgrade' manually to review and apply.`,
	Run: runAdminUpgrade,
}

func runAdminUpgrade(cmd *cobra.Command, _ []string) {
	configDir, _ := cmd.Root().Flags().GetString("config-dir")
	dataDir, _ := cmd.Root().Flags().GetString("spinifex-dir")
	yes, _ := cmd.Flags().GetBool("yes")

	// Check that the installation exists.
	spinifexToml := configDir + "/spinifex.toml"
	if _, err := os.Stat(spinifexToml); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "No Spinifex installation found at %s\nRun 'spx admin init' first.\n", configDir)
		os.Exit(1)
	}

	// Show current versions.
	versions, err := migrate.DefaultRegistry.ConfigVersions(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading config versions: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Reading current config versions...")
	for name, v := range versions {
		if v == 0 {
			fmt.Printf("  %-20s %d (no version marker)\n", name+":", v)
		} else {
			fmt.Printf("  %-20s %d\n", name+":", v)
		}
	}
	fmt.Println()

	// Check for pending migrations.
	pending, err := migrate.DefaultRegistry.PendingConfig(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error checking pending migrations: %v\n", err)
		os.Exit(1)
	}

	if len(pending) == 0 {
		fmt.Println("No pending config migrations.")
		return
	}

	fmt.Println("Pending config migrations:")
	for _, p := range pending {
		fmt.Printf("  [config] %s %d → %d: %s\n", p.Target, p.FromVersion, p.ToVersion, p.Description)
	}
	fmt.Println()

	// Prompt for confirmation unless --yes.
	if !yes {
		fmt.Print("Apply? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Aborted.")
			return
		}
	}

	// Apply migrations.
	if err := migrate.DefaultRegistry.RunAllConfig(configDir, dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "Migration failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nDone. Restart services to apply: sudo systemctl restart spinifex.target")
}
