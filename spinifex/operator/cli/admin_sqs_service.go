package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

// sqsEnableTimeout outlasts the gateway's own wait for the SQS VM, so the
// gateway decides when the launch is reported as pending.
const sqsEnableTimeout = sqsEnsureTimeout

var sqsEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable SQS for the Region on this single-node cluster",
	Long: `Record SQS as enabled for the Region, then launch or report the Region's SQS
VM, which runs the worker and its local store, on --node. The desired state is recorded first, so rerunning the
command resumes an interrupted enable. Only a single-node cluster is supported.

Enabled is not ready: run "spx admin sqs status" to see whether the VM has
recorded its receipt key and its worker is polling.

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
	sqsEnableCmd.Flags().String("instance-type", "", "Instance type for the SQS VM (required)")
	sqsEnableCmd.Flags().Int("volume-size-gib", 0, "SQS data volume size in GiB (required)")
}

// sqsStatusOutput is the EnableSQS and DescribeSQS response body.
type sqsStatusOutput struct {
	Region        string `json:"Region"`
	Desired       string `json:"Desired"`
	NodeID        string `json:"NodeId"`
	Generation    uint64 `json:"Generation"`
	Instance      string `json:"Instance"`
	InstanceID    string `json:"InstanceId"`
	ReceiptKey    string `json:"ReceiptKey"`
	WorkerPolling bool   `json:"WorkerPolling"`
	TenantAPI     string `json:"TenantAPI"`
	Unsupported   string `json:"Unsupported"`
	Ready         bool   `json:"Ready"`
}

func runSQSEnable(cmd *cobra.Command, _ []string) error {
	node, instanceType, size, err := sqsEnsureParams(cmd)
	if err != nil {
		return err
	}
	target, err := resolveAdminTarget(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), sqsEnableTimeout)
	defer cancel()
	out, err := enableSQSRemote(ctx, target, node, instanceType, size)
	if err != nil {
		return err
	}
	renderSQSStatus(os.Stdout, out)
	return nil
}

// enableSQSRemote sends EnableSQS and accepts only an enabled reply for node.
func enableSQSRemote(ctx context.Context, target adminTarget, node, instanceType string, sizeGiB int) (*sqsStatusOutput, error) {
	form := url.Values{
		"Action": {"EnableSQS"}, "NodeId": {node}, "InstanceType": {instanceType},
		"VolumeSizeGiB": {strconv.Itoa(sizeGiB)},
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
	fmt.Fprintf(w, "SQS VM:         %s", out.Instance)
	if out.InstanceID != "" {
		fmt.Fprintf(w, " (%s)", out.InstanceID)
	}
	if out.ReceiptKey != "" {
		fmt.Fprintf(w, "\nReceipt key:    %s", out.ReceiptKey)
	}
	fmt.Fprintf(w, "\nWorker polling: %t\nTenant API:     %s\n", out.WorkerPolling, out.TenantAPI)
	if out.Unsupported != "" {
		fmt.Fprintf(w, "Unsupported:    %s\n", out.Unsupported)
	}
	fmt.Fprintf(w, "Ready:          %t\n", out.Ready)
}
