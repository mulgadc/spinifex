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

A bucket also has to be able to serve, which is a different question from how
many replicas it was configured for. ONLINE counts the replicas that are caught
up and reachable; a bucket whose ONLINE is not a majority of REPLICAS cannot
accept a write no matter what its replica count says.

--repair raises any under-replicated bucket to the cluster's replica count,
capped at JetStream's maximum of five. It changes nothing else about a bucket
and never lowers a replica count, so it is safe to run on a healthy cluster and
safe to run twice.

Exit status separates the two problems, so this can gate a deployment without
parsing the output:

  0  every bucket is at the cluster's replica count and can serve
  1  at least one bucket is under-replicated — --repair is the answer
  2  at least one bucket has no quorum, or the report could not be produced —
     a node needs looking at, and --repair will not help`,
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

	// Anything that stops the report being produced exits unreachable rather
	// than under-replicated: an empty answer is not a cluster with no buckets,
	// and --repair is not the response to it.
	_, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect to cluster: %v\n", err)
		os.Exit(exitUnreachable)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "get JetStream context: %v\n", err)
		os.Exit(exitUnreachable)
	}

	// Background: this runs at CLI top level, where there is no request to
	// inherit a deadline from and the process exits after the one command.
	ctx := context.Background()
	reports, err := kvutil.AuditBucketReplicas(ctx, js)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit KV bucket replicas: %v\n", err)
		os.Exit(exitUnreachable)
	}

	if repair {
		repairReplicaReports(ctx, js, reports, os.Stderr)
	}

	if err := writeReplicaReport(os.Stdout, reports, asJSON); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		os.Exit(exitUnreachable)
	}

	os.Exit(replicaReportExit(reports))
}

// Exit statuses, documented in the command's own help because a deployment gate
// reads them rather than the output.
const (
	exitHealthy = 0
	// exitUnderReplicated is the repairable problem: the buckets serve, but on
	// fewer nodes than the cluster has.
	exitUnderReplicated = 1
	// exitUnreachable is the problem repair cannot touch — a bucket that cannot
	// accept a write, or a report that could not be produced at all.
	exitUnreachable = 2
)

// replicaReportExit is the exit status for a set of reports.
//
// No quorum outranks under-replicated because they need different responses: one
// is fixed by raising a count, the other by finding out why a node is not
// answering. A bucket that is both should send the operator to the node.
func replicaReportExit(reports []kvutil.BucketReport) int {
	status := exitHealthy
	for _, r := range reports {
		if !r.HasQuorum() {
			return exitUnreachable
		}
		if r.UnderReplicated() {
			status = exitUnderReplicated
		}
	}
	return status
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

// underReplicatedCount is how many buckets are below the cluster's replica count.
func underReplicatedCount(reports []kvutil.BucketReport) int {
	under := 0
	for _, r := range reports {
		if r.UnderReplicated() {
			under++
		}
	}
	return under
}

// noQuorumCount is how many buckets cannot currently accept a write.
func noQuorumCount(reports []kvutil.BucketReport) int {
	stuck := 0
	for _, r := range reports {
		if !r.HasQuorum() {
			stuck++
		}
	}
	return stuck
}

// replicaStatus is the one word for a bucket's condition.
//
// No quorum wins over under-replicated when both are true, matching the exit
// status: a bucket that cannot serve is the more urgent of the two and is not
// what --repair is for.
func replicaStatus(r kvutil.BucketReport) string {
	switch {
	case !r.HasQuorum():
		return "NO-QUORUM"
	case r.UnderReplicated():
		return "UNDER-REPLICATED"
	default:
		return "ok"
	}
}

// writeReplicaReport renders reports as a table, or as JSON when asJSON.
func writeReplicaReport(out io.Writer, reports []kvutil.BucketReport, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(reports)
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BUCKET\tREPLICAS\tWANT\tONLINE\tSTATUS\tLEADER\tHELD BY")
	for _, r := range reports {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\t%s\t%s\n",
			r.Bucket, r.Replicas, r.Want, r.Online, replicaStatus(r), r.Leader, strings.Join(r.Peers, ","))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "\n%d buckets, %d under-replicated, %d without quorum\n",
		len(reports), underReplicatedCount(reports), noQuorumCount(reports))
	return err
}
