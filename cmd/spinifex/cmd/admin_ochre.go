package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"

	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/spf13/cobra"
)

var adminOchreCmd = &cobra.Command{
	Use:   "ochre",
	Short: "Manage the Ochre inference service",
}

var adminOchreAccessCmd = &cobra.Command{
	Use:   "access",
	Short: "Manage per-account model access grants",
	Long: `Manage which accounts may see and invoke which Ochre models.

Access is deny-by-default: an account with no grants sees an empty model
catalog and every invocation is refused. Grants are cluster state held in the
bedrock-model-access KV bucket, so these commands need a running cluster but
may be run from any node.`,
}

var adminOchreAccessGrantCmd = &cobra.Command{
	Use:   "grant",
	Short: "Grant an account access to a model",
	Run:   runOchreAccessGrant,
}

var adminOchreAccessRevokeCmd = &cobra.Command{
	Use:   "revoke",
	Short: "Revoke an account's access to a model",
	Run:   runOchreAccessRevoke,
}

var adminOchreAccessListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the models an account has been granted",
	Run:   runOchreAccessList,
}

var adminOchreModelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List every model in the platform catalog",
	Long: `List the full platform catalog, ignoring grants. This is what an operator can
grant from; it is not what any account can see.`,
	Run: runOchreModels,
}

func init() {
	adminCmd.AddCommand(adminOchreCmd)
	adminOchreCmd.AddCommand(adminOchreAccessCmd)
	adminOchreCmd.AddCommand(adminOchreModelsCmd)
	adminOchreAccessCmd.AddCommand(adminOchreAccessGrantCmd)
	adminOchreAccessCmd.AddCommand(adminOchreAccessRevokeCmd)
	adminOchreAccessCmd.AddCommand(adminOchreAccessListCmd)

	for _, c := range []*cobra.Command{adminOchreAccessGrantCmd, adminOchreAccessRevokeCmd} {
		c.Flags().String("account-id", "", "12-digit account ID to change (required)")
		c.Flags().String("model-id", "", "Model ID to change (e.g. meta.llama3-70b-instruct-v1:0)")
		c.Flags().Bool("all-models", false, "Apply to every model in the platform catalog")
		if err := c.MarkFlagRequired("account-id"); err != nil {
			panic(err)
		}
	}

	adminOchreAccessListCmd.Flags().String("account-id", "", "12-digit account ID to inspect (required)")
	if err := adminOchreAccessListCmd.MarkFlagRequired("account-id"); err != nil {
		panic(err)
	}
}

// ochreAccessStore connects to the cluster and returns the grant store along
// with a cleanup that closes the connection.
func ochreAccessStore() (*gateway_bedrock.ModelAccessStore, func(), error) {
	cfg, nc, err := loadConfigAndConnect()
	if err != nil {
		return nil, nil, fmt.Errorf("connect to cluster: %w", err)
	}
	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("jetstream context: %w", err)
	}
	return gateway_bedrock.NewModelAccessStore(js, len(cfg.Nodes)), func() { nc.Close() }, nil
}

// ochreTargetModels resolves the --model-id / --all-models pair into the model
// set to act on. Exactly one of the two must be given: defaulting either way
// would make a mistyped flag silently change more or less than intended.
func ochreTargetModels(cmd *cobra.Command) ([]string, error) {
	modelID, _ := cmd.Flags().GetString("model-id")
	allModels, _ := cmd.Flags().GetBool("all-models")

	switch {
	case allModels && modelID != "":
		return nil, fmt.Errorf("--model-id and --all-models are mutually exclusive")
	case allModels:
		return gateway_bedrock.CatalogModelIDs(), nil
	case modelID != "":
		return []string{modelID}, nil
	default:
		return nil, fmt.Errorf("one of --model-id or --all-models is required")
	}
}

func runOchreAccessGrant(cmd *cobra.Command, _ []string) {
	runOchreAccessChange(cmd, true)
}

func runOchreAccessRevoke(cmd *cobra.Command, _ []string) {
	runOchreAccessChange(cmd, false)
}

// runOchreAccessChange applies a grant or revoke across the resolved model set.
// Both operations are idempotent, so re-running after a partial failure is safe
// and this is callable from provisioning that runs on every deploy.
func runOchreAccessChange(cmd *cobra.Command, grant bool) {
	accountID, _ := cmd.Flags().GetString("account-id")

	models, err := ochreTargetModels(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	store, cleanup, err := ochreAccessStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	ctx := context.Background()
	verb := "Granted"
	for _, modelID := range models {
		if grant {
			err = store.Grant(ctx, accountID, modelID)
		} else {
			verb = "Revoked"
			err = store.Revoke(ctx, accountID, modelID)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ %s %s → %s\n", verb, accountID, modelID)
	}
}

func runOchreAccessList(cmd *cobra.Command, _ []string) {
	accountID, _ := cmd.Flags().GetString("account-id")

	store, cleanup, err := ochreAccessStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	models, err := store.List(context.Background(), accountID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if len(models) == 0 {
		fmt.Printf("Account %s has no model grants (it can see and invoke nothing).\n", accountID)
		return
	}

	// KV key order is not meaningful, so sort for a stable, diffable listing.
	sort.Strings(models)
	for _, modelID := range models {
		fmt.Println(modelID)
	}
}

func runOchreModels(_ *cobra.Command, _ []string) {
	models := gateway_bedrock.CatalogModelIDs()
	sort.Strings(models)
	for _, modelID := range models {
		fmt.Println(modelID)
	}
}
