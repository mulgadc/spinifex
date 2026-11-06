package cli

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2inventory "github.com/mulgadc/spinifex/spinifex/domains/ec2/inventory"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	ekscluster "github.com/mulgadc/spinifex/spinifex/domains/eks/cluster"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go/jetstream"
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
	mustMarkFlagRequired(adminEksRestoreSnapshotCmd, "cluster")
}

func runAdminEksRestoreSnapshot(cmd *cobra.Command, _ []string) {
	clusterName, _ := cmd.Flags().GetString("cluster")
	snapshot, _ := cmd.Flags().GetString("snapshot")
	accountID, _ := cmd.Flags().GetString("account")
	if accountID == "" {
		accountID = awsidentifiers.DefaultAccountID()
	}

	_, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()

	svc := ekscluster.NewNATSEKSService(nc)
	// The server side launches a VM (30-60s) then fences the old CP with retries,
	// so the RPC needs generous headroom over a bare launch.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	fmt.Printf("Restoring control plane for cluster %q (account %s)...\n", clusterName, accountID)
	out, err := svc.RestoreSnapshot(ctx, &ekscluster.RestoreSnapshotInput{
		ClusterName: clusterName,
		Snapshot:    snapshot,
	}, accountID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("New control plane %s launched (snapshot %s)\n", out.NewInstanceID, out.Snapshot)
	fmt.Printf("Status: %s\n", out.Status)
}

var adminEksOrphansCmd = &cobra.Command{
	Use:   "orphans",
	Short: "List running EKS control-plane VMs that have no cluster record",
	Long: `orphans reports each running EKS control-plane VM whose cluster record, or whole
account bucket, is missing (ORPHAN), and each it could not judge because a read
failed (UNREADABLE). It only reads and never creates or removes anything.

It lists the system account's instances through the strict inventory, so if any
configured node does not answer it prints "listing incomplete" and exits non-zero
without a partial list. It cannot say which node was silent. An instance with no
ENI, or whose ENI is gone or not tagged as an EKS control plane, is skipped.

Exit status is non-zero when the listing is incomplete or any row is ORPHAN or
UNREADABLE.`,
	Run: runAdminEksOrphans,
}

func init() {
	adminEksCmd.AddCommand(adminEksOrphansCmd)
}

func runAdminEksOrphans(_ *cobra.Command, _ []string) {
	cfg, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	nodeIDs := slices.Sorted(maps.Keys(cfg.Nodes))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	list := func(ctx context.Context) (*ec2.DescribeInstancesOutput, error) {
		return ec2inventory.NewNATSInstances(nc, nodeIDs).ListAccountInstances(ctx, awsidentifiers.GlobalAccountID)
	}
	eni := ekscluster.NewENITagReader(func(ctx context.Context, id string) (*ec2.DescribeNetworkInterfacesOutput, error) {
		return ec2vpc.NewNATSVPCService(nc).DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
			NetworkInterfaceIds: aws.StringSlice([]string{id}),
		}, awsidentifiers.GlobalAccountID)
	})

	orphans, unreadable, err := ekscluster.ReportControlPlaneOrphans(ctx, os.Stdout, js, list, eni, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%d orphan(s), %d unreadable\n", orphans, unreadable)
	if orphans+unreadable > 0 {
		os.Exit(1)
	}
}
