package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/spf13/cobra"
)

// applianceTeardownTimeout bounds the teardown NATS round trip: it covers an
// RDS DeleteDBInstance call plus a KV delete, not a VM boot, so it is far
// shorter than the appliance's own launch timeout.
const applianceTeardownTimeout = 5 * time.Minute

var ochreApplianceCmd = &cobra.Command{
	Use:   "appliance",
	Short: "Manage the shared platform Postgres appliance backing every Ochre vector index",
}

var ochreApplianceTeardownCmd = &cobra.Command{
	Use:   "teardown",
	Short: "Destroy the shared platform Postgres appliance",
	Long: `teardown destroys the platform appliance singleton: its backing RDS DB
instance, this node's VPC host port, and its KV record.

This is a destructive, cluster-wide operator action -- every account's vector
index data stored in the appliance is lost with it. By default the index
registry and KB/DataSource metadata survive, exactly as they survive a
restart: a future daemon startup re-provisions a fresh appliance (a new
instance, a new password) and reconciles each surviving index by re-creating
it and re-ingesting from its recorded source, with no operator action and no
change to its id. --purge-metadata additionally wipes that metadata for an
intentional full destroy, after which nothing is auto-restored.

Requires --confirm.`,
	Run: runOchreApplianceTeardown,
}

func init() {
	ochreCmd.AddCommand(ochreApplianceCmd)
	ochreApplianceCmd.AddCommand(ochreApplianceTeardownCmd)

	ochreApplianceTeardownCmd.Flags().Bool("confirm", false,
		"Required: confirms you intend to destroy the shared platform appliance and every account's vector index data with it")
	ochreApplianceTeardownCmd.Flags().Bool("purge-metadata", false,
		"Also wipe the index registry and KB/DataSource records, disabling auto-restore on the next re-provision")
}

// applianceTeardownFn indirects the NATS call so runApplianceTeardownGuarded
// is testable without a live daemon, mirroring vectorServiceFn/
// endpointServiceFn.
var applianceTeardownFn = func(ctx context.Context, purgeMetadata bool) error {
	_, nc, err := loadConfigAndConnectFn()
	if err != nil {
		return err
	}
	defer nc.Close()
	_, err = utils.NATSRequest[handlers_ochrevector.TeardownApplianceResponse](ctx, nc,
		handlers_ochrevector.SubjectTeardownAppliance, &handlers_ochrevector.TeardownApplianceRequest{PurgeMetadata: purgeMetadata},
		applianceTeardownTimeout, utils.GlobalAccountID)
	return err
}

// runApplianceTeardownGuarded is the testable core of 'ochre appliance
// teardown': it refuses without --confirm before making any NATS call, so a
// missing flag never even opens a connection to the cluster.
func runApplianceTeardownGuarded(ctx context.Context, confirmed, purgeMetadata bool, teardown func(context.Context, bool) error) (string, error) {
	if !confirmed {
		return "", errors.New("refusing to tear down the shared platform appliance without --confirm: " +
			"this destroys every account's vector index data and is not automatically rebuilt")
	}
	if err := teardown(ctx, purgeMetadata); err != nil {
		return "", err
	}
	if purgeMetadata {
		return "✅ Platform appliance torn down, index/KB metadata purged.", nil
	}
	return "✅ Platform appliance torn down.", nil
}

func runOchreApplianceTeardown(cmd *cobra.Command, _ []string) {
	confirmed, _ := cmd.Flags().GetBool("confirm")
	purgeMetadata, _ := cmd.Flags().GetBool("purge-metadata")

	msg, err := runApplianceTeardownGuarded(context.Background(), confirmed, purgeMetadata, applianceTeardownFn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		ochreExit(1)
		return
	}
	fmt.Println(msg)
}
