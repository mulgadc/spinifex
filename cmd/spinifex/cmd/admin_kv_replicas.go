package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/cobra"
)

var kvReplicasCmd = &cobra.Command{
	Use:   "replicas",
	Short: "Report how many nodes each KV bucket is replicated across",
	Long: `Print one line per JetStream KV bucket: how many replicas it has, how many the
cluster's size entitles it to, and which nodes currently hold it.

A bucket with fewer replicas than the cluster has nodes lives on fewer nodes
than exist, and every node depends on those. Losing one of them makes the
bucket unreadable and unwritable from every node at once, which is a
cluster-wide outage caused by a single-node event.

--repair raises any under-replicated bucket to the cluster's node count,
capped at JetStream's maximum of five. It changes nothing else about a bucket
and never lowers a replica count, so it is safe to run on a healthy cluster and
safe to run twice.

Exit status is 1 when any bucket is under-replicated, so this can gate a
deployment without parsing the output.`,
	Run: runKVReplicas,
}

func init() {
	kvCmd.AddCommand(kvReplicasCmd)
	kvReplicasCmd.Flags().Bool("repair", false, "raise every under-replicated bucket to the cluster's node count")
	kvReplicasCmd.Flags().Bool("json", false, "emit the report as JSON")
}

func runKVReplicas(cmd *cobra.Command, _ []string) {
	repair, _ := cmd.Flags().GetBool("repair")
	asJSON, _ := cmd.Flags().GetBool("json")

	_, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect to cluster: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "get JetStream context: %v\n", err)
		os.Exit(1)
	}

	// Background: this runs at CLI top level, where there is no request to
	// inherit a deadline from and the process exits after the one command.
	ctx := context.Background()
	reports, err := kvutil.AuditBucketReplicas(ctx, js)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit KV bucket replicas: %v\n", err)
		os.Exit(1)
	}

	if repair {
		repairReplicaReports(ctx, js, reports, os.Stderr)
	}

	if err := writeReplicaReport(os.Stdout, reports, asJSON); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		os.Exit(1)
	}

	if underReplicatedCount(reports) > 0 {
		os.Exit(1)
	}
}

// repairReplicaReports raises every under-replicated bucket and updates reports
// in place for the ones that were raised.
//
// One bucket that cannot be placed must not stop the others being repaired, so a
// failure is named on errOut and the sweep continues. The bucket it failed on
// stays UNDER-REPLICATED in the report, which is already what makes the exit
// status non-zero, so nothing is lost by not returning an error here.
func repairReplicaReports(ctx context.Context, js jetstream.JetStream, reports []kvutil.BucketReport, errOut io.Writer) {
	for i, r := range reports {
		if !r.UnderReplicated() {
			continue
		}
		if err := kvutil.RaiseBucketReplicas(ctx, js, r.Bucket, r.Want); err != nil {
			fmt.Fprintf(errOut, "repair %s: %v\n", r.Bucket, err)
			continue
		}
		reports[i].Replicas = r.Want
	}
}

// underReplicatedCount is how many buckets are below the cluster's replica
// count, which is what the command's exit status reports.
func underReplicatedCount(reports []kvutil.BucketReport) int {
	under := 0
	for _, r := range reports {
		if r.UnderReplicated() {
			under++
		}
	}
	return under
}

// writeReplicaReport renders reports as a table, or as JSON when asJSON.
func writeReplicaReport(out io.Writer, reports []kvutil.BucketReport, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(reports)
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BUCKET\tREPLICAS\tWANT\tSTATUS\tHELD BY")
	for _, r := range reports {
		status := "ok"
		if r.UnderReplicated() {
			status = "UNDER-REPLICATED"
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n", r.Bucket, r.Replicas, r.Want, status, strings.Join(r.Peers, ","))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "\n%d buckets, %d under-replicated\n", len(reports), underReplicatedCount(reports))
	return err
}
