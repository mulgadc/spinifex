package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/accountteardown"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/spf13/cobra"
)

// accountDeleteTimeout is the outer bound on the whole teardown. It must stay
// well above the sum of the per-stage drain budgets, which are what actually
// decide when to give up on a resource; cutting in before them would abandon a
// large account midway with no stuck report to show for it.
const accountDeleteTimeout = 2 * time.Hour

var accountDeleteCmd = &cobra.Command{
	Use:   "delete <account-id>",
	Short: "Tear down an account's infrastructure and remove the account",
	Long: `Remove an account and everything it owns.

The account is marked TERMINATING first, which blocks its credentials, and then
its resources are deleted in dependency order: compute, attachments, storage,
network, platform, identity, account. Nothing is removed until the account can
no longer create more.

The account name must be given to confirm, so a mistyped account id fails
rather than emptying a live tenant. The system account (000000000000) and the
super admin (000000000001) can never be deleted.

  spx admin account delete 000000000042 --dry-run
  spx admin account delete 000000000042 --name tenant@example.com

--force is for resources the ordinary API refuses to delete: a volume attached
to an instance that will not terminate leaves both stranded and the account can
never be emptied. It clears the attachment in the control plane without the
guest's cooperation, so use it only once the guest is being destroyed.`,
	Args: cobra.ExactArgs(1),
	RunE: runAccountDelete,

	SilenceUsage: true,
}

func init() {
	accountCmd.AddCommand(accountDeleteCmd)

	accountDeleteCmd.Flags().Bool("dry-run", false, "Report what would be deleted and change nothing")
	accountDeleteCmd.Flags().Bool("force", false, "Escalate past state guards for resources that will not delete")
	accountDeleteCmd.Flags().Bool("yes", false, "Skip the interactive confirmation (requires --name)")
	accountDeleteCmd.Flags().String("name", "", "Account name, which must match the stored record")
	accountDeleteCmd.Flags().Bool("remote", false, "Delete over POST /admin/DeleteAccount instead of connecting to NATS")
	accountDeleteCmd.Flags().String("endpoint", "", "Gateway endpoint for --remote (default: this node's AWS gateway)")
	accountDeleteCmd.Flags().String("region", "", "SigV4 region for --remote (default: this node's region)")
	accountDeleteCmd.Flags().String("ca-bundle", "", "CA certificate for --remote (default: this node's CA)")
	accountDeleteCmd.Flags().String("client-token", "", "Idempotency token for --remote (default: generated; reuse it to follow the same teardown)")
}

func runAccountDelete(cmd *cobra.Command, args []string) error {
	accountID := args[0]
	if remote, _ := cmd.Flags().GetBool("remote"); remote {
		return runAccountDeleteRemote(cmd, accountID)
	}

	dryRun, _ := cmd.Flags().GetBool("dry-run")
	force, _ := cmd.Flags().GetBool("force")
	assumeYes, _ := cmd.Flags().GetBool("yes")
	name, _ := cmd.Flags().GetString("name")

	svc, cfg, nc, cleanup, err := initIAMServiceFromConfig()
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(cmd.Context(), accountDeleteTimeout)
	defer cancel()

	node := cfg.Nodes[cfg.Node]
	buckets := objectstore.NewS3ObjectStoreFromConfig(
		admin.DialTarget(node.Predastore.Host),
		node.Predastore.Region,
		node.Predastore.AccessKey,
		node.Predastore.SecretKey,
	)

	engine, err := accountteardown.NewClusterEngine(ctx, nc, len(cfg.Nodes), svc, buckets)
	if err != nil {
		return err
	}

	if dryRun {
		result, err := engine.Inventory(ctx, accountID)
		if err != nil {
			return err
		}
		printTeardownPlan(result)
		fmt.Printf("\nDry run: nothing was deleted. %d resources would be removed.\n", result.DeletedCount())
		return nil
	}

	// Always show the inventory before destroying it. An operator who has not
	// seen what is about to go has not really confirmed anything.
	plan, err := engine.Inventory(ctx, accountID)
	if err != nil {
		return err
	}
	printTeardownPlan(plan)

	if name == "" {
		if assumeYes {
			return errors.New("--yes requires --name so the account is still confirmed")
		}
		name, err = promptAccountName(accountID)
		if err != nil {
			return err
		}
	}

	result, err := engine.Teardown(ctx, accountteardown.Request{
		AccountID: accountID, AccountName: name, Force: force,
	})
	if result != nil {
		printTeardownResult(result)
	}
	if errors.Is(err, accountteardown.ErrResourcesStuck) {
		fmt.Fprintf(os.Stderr, "\nThe account is still TERMINATING so its leftovers keep an owner.\n")
		if !force {
			fmt.Fprintf(os.Stderr, "Re-run with --force to clear resources the ordinary API refuses to delete.\n")
		}
		return err
	}
	if err != nil {
		return err
	}

	fmt.Printf("\nAccount %s deleted. %d resources removed.\n", accountID, result.DeletedCount())
	return nil
}

// promptAccountName asks for the confirmation name on a terminal.
func promptAccountName(accountID string) (string, error) {
	fmt.Printf("\nThis permanently deletes account %s and everything above.\n", accountID)
	fmt.Print("Type the account name to confirm: ")

	reader := bufio.NewReader(os.Stdin)
	entered, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read confirmation: %w", err)
	}
	entered = strings.TrimSpace(entered)
	if entered == "" {
		return "", errors.New("cancelled: no account name given")
	}
	return entered, nil
}

func printTeardownPlan(result *accountteardown.Result) {
	fmt.Printf("Account %s (%s)\n\n", result.AccountID, result.AccountName)
	if result.DeletedCount() == 0 {
		fmt.Println("The account holds no resources.")
		return
	}
	for _, stage := range result.Stages {
		if len(stage.Deleted) == 0 {
			continue
		}
		fmt.Printf("  %s\n", stage.Stage)
		for _, resource := range stage.Deleted {
			fmt.Printf("    %s\n", resource)
		}
	}
}

func printTeardownResult(result *accountteardown.Result) {
	fmt.Println()
	for _, stage := range result.Stages {
		printTeardownStage(stage)
	}
}

// printTeardownStage reports one finished stage. A stage that removed nothing
// and left nothing behind is not printed: an account with no ECS clusters
// should not read as seven lines of activity.
func printTeardownStage(stage accountteardown.StageResult) {
	if len(stage.Deleted) == 0 && len(stage.Stuck) == 0 {
		return
	}
	fmt.Printf("  %-12s %d deleted, %d stuck (%s)\n",
		stage.Stage, len(stage.Deleted), len(stage.Stuck), stage.Elapsed)
	for _, stuck := range stage.Stuck {
		fmt.Printf("    STUCK %s: %s\n", stuck.Resource, stuck.Reason)
	}
}
