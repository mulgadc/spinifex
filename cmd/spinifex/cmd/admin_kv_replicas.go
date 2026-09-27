package cmd

import (
	"context"
	"encoding/json"
	"fmt"
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
		for i, r := range reports {
			if !r.UnderReplicated() {
				continue
			}
			if err := kvutil.RaiseBucketReplicas(ctx, js, r.Bucket, r.Want); err != nil {
				fmt.Fprintf(os.Stderr, "repair %s: %v\n", r.Bucket, err)
				os.Exit(1)
			}
			reports[i].Replicas = r.Want
		}
	}

	under := 0
	for _, r := range reports {
		if r.UnderReplicated() {
			under++
		}
	}

	if asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(reports); err != nil {
			fmt.Fprintf(os.Stderr, "encode report: %v\n", err)
			os.Exit(1)
		}
	} else {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "BUCKET\tREPLICAS\tWANT\tSTATUS\tHELD BY")
		for _, r := range reports {
			status := "ok"
			if r.UnderReplicated() {
				status = "UNDER-REPLICATED"
			}
			fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n", r.Bucket, r.Replicas, r.Want, status, strings.Join(r.Peers, ","))
		}
		w.Flush()
		fmt.Printf("\n%d buckets, %d under-replicated\n", len(reports), under)
	}

	if under > 0 {
		os.Exit(1)
	}
}
