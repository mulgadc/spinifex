package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon"
	"github.com/spf13/cobra"
)

var (
	rollbackTo      int
	rollbackCancel  bool
	rollbackTimeout time.Duration
)

var clusterRollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Lower the cluster feature level so a previous build can be installed",
	Long: `rollback --to N records the rollback, waits until every configured member has
read it and has no work admitted above level N, runs the drain check of every
level being given up, then lowers the level. The hold stays until release-hold.
A failure leaves the rollback recorded: rerun the same command to resume, or
use --cancel to abandon it (refused once the level has been lowered).`,
	Run: runClusterRollback,
}

var clusterReleaseHoldCmd = &cobra.Command{
	Use:   "release-hold",
	Short: "Allow the cluster feature level to advance again after a rollback",
	Run:   runClusterReleaseHold,
}

func init() {
	clusterCmd.AddCommand(clusterRollbackCmd, clusterReleaseHoldCmd)
	clusterRollbackCmd.Flags().IntVar(&rollbackTo, "to", -1, "feature level to roll back to")
	clusterRollbackCmd.Flags().BoolVar(&rollbackCancel, "cancel", false, "abandon a rollback that has not yet lowered the level")
	clusterRollbackCmd.Flags().DurationVar(&rollbackTimeout, "timeout", featurelevel.DefaultAckTimeout, "how long to wait for every member to acknowledge")
}

// registerDrainChecks is the composition root for drain checks: every declared
// level above the baseline registers its check here, and a level left without
// one stops a rollback from running.
func registerDrainChecks() error {
	if missing := featurelevel.UncoveredLevels(); len(missing) > 0 {
		return fmt.Errorf("feature levels %v have no drain check registered; this build cannot roll back safely", missing)
	}
	return nil
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

func runClusterRollback(cmd *cobra.Command, _ []string) {
	if !rollbackCancel && rollbackTo < 0 {
		fail(errors.New("--to is required (or --cancel)"))
	}
	if err := registerDrainChecks(); err != nil {
		fail(err)
	}
	cfg, nc, err := loadConfigAndConnect()
	if err != nil {
		fail(err)
	}
	defer nc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout+5*time.Minute)
	defer cancel()
	jsm, err := daemon.NewJetStreamManager(nc)
	if err == nil {
		err = jsm.InitClusterConfigBucket()
	}
	if err == nil {
		err = jsm.InitClusterStateBucket()
	}
	if err != nil {
		fail(err)
	}
	store, err := jsm.FeatureLevelStore(ctx)
	if err != nil {
		fail(err)
	}

	if rollbackCancel {
		rec, cerr := store.CancelRollback(ctx)
		if cerr != nil {
			fail(cancelError(cerr))
		}
		fmt.Printf("Rollback cancelled; feature level stays %d.\n", rec.Level)
		return
	}

	nodes := slices.Sorted(maps.Keys(cfg.Nodes))
	probe := featurelevel.NewProbe(nc, nodes)
	rec, err := featurelevel.RunRollback(ctx, rollbackTo, featurelevel.RollbackDeps{
		Store:      store,
		Probe:      probe.Probe,
		Drains:     featurelevel.DrainChecks,
		Build:      featurelevel.Build,
		AckTimeout: rollbackTimeout,
	})
	if err != nil {
		fail(err)
	}
	fmt.Printf("Feature level lowered to %d. The previous build may now be installed.\n", rec.Level)
	fmt.Printf("The hold at %d stays until you run: spx admin cluster release-hold\n", rec.Hold.Level)
}

func cancelError(err error) error {
	if errors.Is(err, featurelevel.ErrNoRollback) {
		return errors.New("no rollback is in progress; if it already finished the level has been lowered and cannot be cancelled")
	}
	return err
}

func runClusterReleaseHold(_ *cobra.Command, _ []string) {
	_, nc, err := loadConfigAndConnect()
	if err != nil {
		fail(err)
	}
	defer nc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	jsm, err := daemon.NewJetStreamManager(nc)
	if err == nil {
		err = jsm.InitClusterConfigBucket()
	}
	if err == nil {
		err = jsm.InitClusterStateBucket()
	}
	if err != nil {
		fail(err)
	}
	store, err := jsm.FeatureLevelStore(ctx)
	if err != nil {
		fail(err)
	}
	rec, err := store.ReleaseHold(ctx)
	if err != nil {
		fail(err)
	}
	fmt.Printf("Hold released; the feature level (%d) may advance once every member is current.\n", rec.Level)
}
