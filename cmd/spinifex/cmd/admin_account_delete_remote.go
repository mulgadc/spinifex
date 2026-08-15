package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mulgadc/spinifex/spinifex/accountteardown"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/spf13/cobra"
)

// accountDeletePollInterval is how often --remote asks the gateway how far the
// teardown has got. The work is minutes long, so a tighter poll would only add
// load without telling the operator anything new.
const accountDeletePollInterval = 10 * time.Second

// runAccountDeleteRemote drives POST /admin/DeleteAccount and then follows the
// job to completion, so the remote path reports the same outcome the local one
// prints rather than only that the teardown started.
func runAccountDeleteRemote(cmd *cobra.Command, accountID string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	force, _ := cmd.Flags().GetBool("force")
	assumeYes, _ := cmd.Flags().GetBool("yes")
	name, _ := cmd.Flags().GetString("name")
	clientToken, _ := cmd.Flags().GetString("client-token")

	target, err := resolveAdminTarget(cmd)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), accountDeleteTimeout)
	defer cancel()

	// The inventory is fetched first even for a real deletion: an operator who
	// has not seen what is about to go has not really confirmed anything.
	plan, err := deleteAccountRemote(ctx, target, gateway.DeleteAccountRequest{
		AccountID: accountID, DryRun: true,
	})
	if err != nil {
		return err
	}
	if plan.Inventory != nil {
		printTeardownPlan(plan.Inventory)
	}
	if dryRun {
		fmt.Printf("\nDry run: nothing was deleted. %d resources would be removed.\n", plan.Inventory.DeletedCount())
		return nil
	}

	if name == "" {
		if assumeYes {
			return errors.New("--yes requires --name so the account is still confirmed")
		}
		if name, err = promptAccountName(accountID); err != nil {
			return err
		}
	}
	if clientToken == "" {
		clientToken = newClientToken()
	}

	started, err := deleteAccountRemote(ctx, target, gateway.DeleteAccountRequest{
		AccountID: accountID, AccountName: name, ClientToken: clientToken, Force: force,
	})
	if err != nil {
		var adminErr *adminError
		if errors.As(err, &adminErr) && retryableAdminErrors[adminErr.Code] {
			fmt.Fprintf(os.Stderr, "Retry with --client-token %s to follow the same teardown.\n", clientToken)
		}
		return err
	}
	fmt.Printf("\nTeardown started: %s\n", started.DeletionID)

	return followAccountDeletion(ctx, target, accountID)
}

// followAccountDeletion polls until the job reaches a terminal state, printing
// each stage as it lands.
func followAccountDeletion(ctx context.Context, target adminTarget, accountID string) error {
	reported := 0
	for {
		job, err := describeAccountDeletionRemote(ctx, target, accountID)
		if err != nil {
			return err
		}

		for ; reported < len(job.Stages); reported++ {
			printTeardownStage(job.Stages[reported])
		}

		switch job.State {
		case gateway.DeletionStateCompleted:
			fmt.Printf("\nAccount %s deleted.\n", accountID)
			return nil
		case gateway.DeletionStateFailed:
			fmt.Fprintf(os.Stderr, "\nThe account is still TERMINATING so its leftovers keep an owner.\n")
			return fmt.Errorf("teardown failed: %s", job.Error)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped following teardown of %s: %w", accountID, ctx.Err())
		case <-time.After(accountDeletePollInterval):
		}
	}
}

func deleteAccountRemote(
	ctx context.Context,
	target adminTarget,
	req gateway.DeleteAccountRequest,
) (*gateway.DeleteAccountResponse, error) {
	var out gateway.DeleteAccountResponse
	if err := callAdmin(ctx, target, "DeleteAccount", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func describeAccountDeletionRemote(ctx context.Context, target adminTarget, accountID string) (*accountDeletionStatus, error) {
	var out accountDeletionStatus
	err := callAdmin(ctx, target, "DescribeAccountDeletion",
		gateway.DescribeAccountDeletionRequest{AccountID: accountID}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// accountDeletionStatus is the subset of the stored job this command reports.
// The gateway's own record type is unexported, and the CLI needs no more than
// the state, the stages and why it stopped.
type accountDeletionStatus struct {
	DeletionID string                        `json:"deletionId"`
	State      string                        `json:"state"`
	Stages     []accountteardown.StageResult `json:"stages"`
	Error      string                        `json:"error"`
}
