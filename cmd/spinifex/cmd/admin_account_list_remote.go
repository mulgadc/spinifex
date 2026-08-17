package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/gateway"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/spf13/cobra"
)

// runAccountListRemote reads the listing over POST /admin/ListAccounts, which
// is how an off-cluster operator or the load-test harness sees the tenants it
// created without SSH access to a node.
func runAccountListRemote(cmd *cobra.Command) error {
	target, err := resolveAdminTarget(cmd)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), adminRequestTimeout)
	defer cancel()

	var out gateway.ListAccountsResponse
	if err := callAdmin(ctx, target, "ListAccounts", struct{}{}, &out); err != nil {
		return err
	}

	printAccountTable(out.Accounts)
	return nil
}

// accountSummaries adapts the local listing to the shape the remote one
// returns, so both paths print through one function and cannot drift.
func accountSummaries(accounts []*handlers_iam.Account) []gateway.AccountSummary {
	summaries := make([]gateway.AccountSummary, 0, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		summaries = append(summaries, gateway.AccountSummary{
			AccountID:   account.AccountID,
			AccountName: account.AccountName,
			Status:      account.Status,
			CreatedAt:   account.CreatedAt,
		})
	}
	return summaries
}

func printAccountTable(accounts []gateway.AccountSummary) {
	if len(accounts) == 0 {
		fmt.Println("No accounts found.")
		return
	}

	fmt.Printf("%-14s %-30s %-12s %s\n", "ACCOUNT ID", "NAME", "STATUS", "CREATED")
	fmt.Printf("%-14s %-30s %-12s %s\n", "----------", "----", "------", "-------")
	for _, account := range accounts {
		created := account.CreatedAt
		if parsed, err := time.Parse(time.RFC3339, account.CreatedAt); err == nil {
			created = parsed.Format("2006-01-02 15:04")
		}
		fmt.Printf("%-14s %-30s %-12s %s\n", account.AccountID, account.AccountName, account.Status, created)
	}
}
