package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/spf13/cobra"
)

var adminEksCmd = &cobra.Command{
	Use:   "eks",
	Short: "Manage EKS control-plane disaster recovery",
}

var adminEksRestoreSnapshotCmd = &cobra.Command{
	Use:   "restore-snapshot",
	Short: "Rebuild a single-CP cluster's control plane from an etcd snapshot",
	Long: `restore-snapshot drives the single-CP total-loss DR path: it launches a fresh
control-plane VM as a cluster-init seed, directs its boot-time recovery agent to
run k3s server --cluster-reset (restoring the given etcd snapshot, or the latest
one found in predastore when --snapshot is omitted), and re-points the cluster
NLB target groups at the new CP.

Only single-CP clusters are supported. An HA cluster has a potentially
surviving etcd quorum and should be recovered via quorum reformation instead
of this destructive single-node rebuild.`,
	Run: runAdminEksRestoreSnapshot,
}

func init() {
	adminCmd.AddCommand(adminEksCmd)
	adminEksCmd.AddCommand(adminEksRestoreSnapshotCmd)

	adminEksRestoreSnapshotCmd.Flags().String("cluster", "", "Cluster name to restore (required)")
	adminEksRestoreSnapshotCmd.Flags().String("snapshot", "", "Etcd snapshot key to restore; defaults to the latest snapshot found in predastore")
	adminEksRestoreSnapshotCmd.Flags().String("account", "", "Account ID that owns the cluster; defaults to the bootstrap account")
	_ = adminEksRestoreSnapshotCmd.MarkFlagRequired("cluster")
}

func runAdminEksRestoreSnapshot(cmd *cobra.Command, _ []string) {
	clusterName, _ := cmd.Flags().GetString("cluster")
	snapshot, _ := cmd.Flags().GetString("snapshot")
	accountID, _ := cmd.Flags().GetString("account")
	if accountID == "" {
		accountID = admin.DefaultAccountID()
	}

	_, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()

	svc := handlers_eks.NewNATSEKSService(nc)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Printf("Restoring control plane for cluster %q (account %s)...\n", clusterName, accountID)
	out, err := svc.RestoreSnapshot(ctx, &handlers_eks.RestoreSnapshotInput{
		ClusterName: clusterName,
		Snapshot:    snapshot,
	}, accountID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Control plane restored: new instance %s, snapshot %s\n", out.NewInstanceID, out.Snapshot)
}
