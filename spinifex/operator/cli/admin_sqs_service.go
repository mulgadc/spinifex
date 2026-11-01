package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// sqsEnableTimeout outlasts the gateway's worker and store waits run back to
// back, so the gateway decides when either launch is reported as pending.
const sqsEnableTimeout = 21 * time.Minute

var sqsEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable SQS for the Region on this single-node cluster",
	Long: `Record SQS as enabled for the Region, then launch or report the SQS worker and
store VMs on --node. The desired state is recorded first, so rerunning the
command resumes an interrupted enable. Only a single-node cluster is supported.

Enabled is not ready: run "spx admin sqs status" to see whether the store holds
its server key and a worker is polling.

Do not put tenant production data in SQS: SQS-managed message encryption is not
implemented, queues cannot be deleted or purged, and nothing removes SQS data yet.`,
	Args:          cobra.NoArgs,
	RunE:          runSQSEnable,
	SilenceUsage:  true,
	SilenceErrors: true,
}

var sqsStatusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Show the Region's requested SQS state and its actual readiness",
	Args:          cobra.NoArgs,
	RunE:          runSQSStatus,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	sqsCmd.AddCommand(sqsEnableCmd, sqsStatusCmd)
	for _, c := range []*cobra.Command{sqsEnableCmd, sqsStatusCmd} {
		c.Flags().String("endpoint", "", "Gateway endpoint (default: this node's AWS gateway)")
		c.Flags().String("region", "", "SigV4 region (default: this node's region)")
		c.Flags().String("ca-bundle", "", "CA certificate (default: this node's CA)")
	}
	sqsEnableCmd.Flags().String("node", "", "The cluster's node (required)")
	sqsEnableCmd.Flags().String("worker-instance-type", "", "Instance type for the worker VM (required)")
	sqsEnableCmd.Flags().String("store-instance-type", "", "Instance type for the store VM (required)")
	sqsEnableCmd.Flags().Int("volume-size-gib", 0, "Store data volume size in GiB (required)")
}

// sqsStatusOutput is the EnableSQS and DescribeSQS response body.
type sqsStatusOutput struct {
	Region          string `json:"Region"`
	Desired         string `json:"Desired"`
	NodeID          string `json:"NodeId"`
	Generation      uint64 `json:"Generation"`
	Store           string `json:"Store"`
	StoreInstanceID string `json:"StoreInstanceId"`
	WorkerPolling   bool   `json:"WorkerPolling"`
	TenantAPI       string `json:"TenantAPI"`
	Ready           bool   `json:"Ready"`
}

func runSQSEnable(cmd *cobra.Command, _ []string) error {
	flag := func(name string) string { v, _ := cmd.Flags().GetString(name); return strings.TrimSpace(v) }
	node, worker, store := flag("node"), flag("worker-instance-type"), flag("store-instance-type")
	size, _ := cmd.Flags().GetInt("volume-size-gib")
	switch {
	case node == "":
		return errors.New("--node is required")
	case worker == "" || store == "":
		return errors.New("--worker-instance-type and --store-instance-type are required")
	case size <= 0:
		return errors.New("--volume-size-gib is required and must be a positive number of GiB")
	}
	target, err := resolveAdminTarget(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), sqsEnableTimeout)
	defer cancel()
	out, err := enableSQSRemote(ctx, target, node, worker, store, size)
	if err != nil {
		return err
	}
	renderSQSStatus(os.Stdout, out)
	return nil
}

// enableSQSRemote sends EnableSQS and accepts only an enabled reply for node.
func enableSQSRemote(ctx context.Context, target adminTarget, node, worker, store string, sizeGiB int) (*sqsStatusOutput, error) {
	form := url.Values{
		"Action": {"EnableSQS"}, "NodeId": {node}, "WorkerInstanceType": {worker},
		"StoreInstanceType": {store}, "VolumeSizeGiB": {strconv.Itoa(sizeGiB)},
	}
	var out sqsStatusOutput
	if err := callSpinifexAction(ctx, target, form, &out); err != nil {
		return nil, err
	}
	if out.Desired != "enabled" || out.NodeID != node {
		return nil, fmt.Errorf("gateway returned an unexpected enable reply (desired %q, node %q)", out.Desired, out.NodeID)
	}
	return &out, nil
}

func runSQSStatus(cmd *cobra.Command, _ []string) error {
	target, err := resolveAdminTarget(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
	defer cancel()
	var out sqsStatusOutput
	if err := callSpinifexAction(ctx, target, url.Values{"Action": {"DescribeSQS"}}, &out); err != nil {
		return err
	}
	renderSQSStatus(os.Stdout, &out)
	return nil
}

func renderSQSStatus(w io.Writer, out *sqsStatusOutput) {
	fmt.Fprintf(w, "Region:         %s\nRequested:      %s\n", out.Region, out.Desired)
	if out.NodeID != "" {
		fmt.Fprintf(w, "Node:           %s\n", out.NodeID)
	}
	fmt.Fprintf(w, "Store:          %s", out.Store)
	if out.StoreInstanceID != "" {
		fmt.Fprintf(w, " (%s)", out.StoreInstanceID)
	}
	fmt.Fprintf(w, "\nWorker polling: %t\nTenant API:     %s\nReady:          %t\n", out.WorkerPolling, out.TenantAPI, out.Ready)
}
