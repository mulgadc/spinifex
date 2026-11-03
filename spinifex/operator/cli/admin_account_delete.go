package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	accountteardown "github.com/mulgadc/spinifex/spinifex/operator/accountlifecycle"
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
guest's cooperation, so use it only once the guest is being destroyed.

The deletion always runs through the gateway's admin API (POST /admin/DeleteAccount,
then DescribeAccountDeletion), signed with the standard AWS credential chain. On a cluster node that
means AWS_PROFILE=spinifex; without it no credentials are found. The caller
must hold spinifex:DeleteAccount and spinifex:DescribeAccountDeletion.
--remote is accepted for existing scripts and has no effect.`,
	Args: cobra.ExactArgs(1),
	RunE: runAccountDelete,

	SilenceUsage: true,
}

// runAccountDelete always deletes through the gateway's admin API. The engine
// is never built in this process, so a CLI delete cannot race a job's holder.
func runAccountDelete(cmd *cobra.Command, args []string) error {
	return runAccountDeleteRemote(cmd, args[0])
}

func init() {
	accountCmd.AddCommand(accountDeleteCmd)

	accountDeleteCmd.Flags().Bool("dry-run", false, "Report what would be deleted and change nothing")
	accountDeleteCmd.Flags().Bool("non-finalizing", false, "Quiesce and clean the EC2 platform kinds, then stop with the account still TERMINATING")
	_ = accountDeleteCmd.Flags().MarkHidden("non-finalizing")
	accountDeleteCmd.Flags().Bool("force", false, "Escalate past state guards for resources that will not delete")
	accountDeleteCmd.Flags().Bool("yes", false, "Skip the interactive confirmation (requires --name)")
	accountDeleteCmd.Flags().String("name", "", "Account name, which must match the stored record")
	accountDeleteCmd.Flags().Bool("remote", false, "Accepted for compatibility and ignored: deletion always goes through POST /admin/DeleteAccount")
	accountDeleteCmd.Flags().String("endpoint", "", "Gateway endpoint (default: this node's AWS gateway)")
	accountDeleteCmd.Flags().String("region", "", "SigV4 region (default: this node's region)")
	accountDeleteCmd.Flags().String("ca-bundle", "", "CA certificate (default: this node's CA)")
	accountDeleteCmd.Flags().String("client-token", "", "Idempotency token (default: generated; reuse it to follow the same teardown)")
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
