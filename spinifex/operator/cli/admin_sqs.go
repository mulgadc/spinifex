package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/spf13/cobra"
)

// sqsEnsureTimeout outlasts the gateway's own ten-minute wait for the node, so
// the gateway, not this client, decides when a launch is reported as pending.
const sqsEnsureTimeout = 11 * time.Minute

const sqsBoundState = "bound"

var sqsCmd = &cobra.Command{
	Use:   "sqs",
	Short: "Launch the SQS system worker and store on a chosen node",
}

var sqsEnsureWorkerCmd = &cobra.Command{
	Use:   "ensure-worker",
	Short: "Launch or report the Region's SQS worker VM on a node",
	Long: `Launch the Region's shared SQS worker VM on --node, or report the VM already
bound there. The request goes to the AWS gateway as the signed spinifex
EnsureSQSWorker action, using the standard AWS credential chain
(e.g. AWS_PROFILE=spinifex); only a super-admin may call it.

A bound VM is not a ready worker: this command does not check SQS or the store.`,
	Args:          cobra.NoArgs,
	RunE:          runSQSEnsureWorker,
	SilenceUsage:  true,
	SilenceErrors: true,
}

var sqsEnsureStoreCmd = &cobra.Command{
	Use:   "ensure-store",
	Short: "Launch or report the Region's SQS store VM on the worker's node",
	Long: `Launch the Region's SQS store VM on --node, which must be the node the worker
is bound to, or report the VM already bound there. The request goes to the AWS
gateway as the signed spinifex EnsureSQSStore action, using the standard AWS
credential chain; only a super-admin may call it.

A bound VM is not a ready store: the worker checks readiness itself.`,
	Args:          cobra.NoArgs,
	RunE:          runSQSEnsureStore,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	adminCmd.AddCommand(sqsCmd)
	sqsCmd.AddCommand(sqsEnsureWorkerCmd, sqsEnsureStoreCmd)

	for _, c := range []*cobra.Command{sqsEnsureWorkerCmd, sqsEnsureStoreCmd} {
		c.Flags().String("node", "", "Node to launch on (required)")
		c.Flags().String("instance-type", "", "Instance type for the VM (required)")
		c.Flags().String("endpoint", "", "Gateway endpoint (default: this node's AWS gateway)")
		c.Flags().String("region", "", "SigV4 region (default: this node's region)")
		c.Flags().String("ca-bundle", "", "CA certificate (default: this node's CA)")
	}
	sqsEnsureStoreCmd.Flags().Int("volume-size-gib", 0, "Store data volume size in GiB (required)")
}

// sqsWorkerEnsureOutput is the EnsureSQSWorker response body.
type sqsWorkerEnsureOutput struct {
	NodeID     string `json:"NodeId"`
	Region     string `json:"Region"`
	InstanceID string `json:"InstanceId"`
	Generation uint64 `json:"Generation"`
	State      string `json:"State"`
}

// sqsStoreEnsureOutput is the EnsureSQSStore response body.
type sqsStoreEnsureOutput struct {
	NodeID       string `json:"NodeId"`
	Region       string `json:"Region"`
	InstanceID   string `json:"InstanceId"`
	Address      string `json:"Address"`
	InstanceType string `json:"InstanceType"`
	State        string `json:"State"`
}

// sqsEnsureParams reads the flags both commands share. Empty values are
// refused here so a typo never reaches the gateway as a launch request.
func sqsEnsureParams(cmd *cobra.Command) (node, instanceType string, err error) {
	node, _ = cmd.Flags().GetString("node")
	instanceType, _ = cmd.Flags().GetString("instance-type")
	node, instanceType = strings.TrimSpace(node), strings.TrimSpace(instanceType)
	if node == "" {
		return "", "", errors.New("--node is required")
	}
	if instanceType == "" {
		return "", "", errors.New("--instance-type is required")
	}
	return node, instanceType, nil
}

func runSQSEnsureWorker(cmd *cobra.Command, _ []string) error {
	node, instanceType, err := sqsEnsureParams(cmd)
	if err != nil {
		return err
	}
	target, err := resolveAdminTarget(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), sqsEnsureTimeout)
	defer cancel()

	out, err := ensureSQSWorkerRemote(ctx, target, node, instanceType)
	if err != nil {
		return err
	}
	renderSQSWorkerEnsure(os.Stdout, out)
	return nil
}

func runSQSEnsureStore(cmd *cobra.Command, _ []string) error {
	node, instanceType, err := sqsEnsureParams(cmd)
	if err != nil {
		return err
	}
	size, _ := cmd.Flags().GetInt("volume-size-gib")
	if size <= 0 {
		return errors.New("--volume-size-gib is required and must be a positive number of GiB")
	}
	target, err := resolveAdminTarget(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), sqsEnsureTimeout)
	defer cancel()

	out, err := ensureSQSStoreRemote(ctx, target, node, instanceType, size)
	if err != nil {
		return err
	}
	renderSQSStoreEnsure(os.Stdout, out)
	return nil
}

// ensureSQSWorkerRemote sends EnsureSQSWorker and accepts only a bound VM on
// the requested node.
func ensureSQSWorkerRemote(ctx context.Context, target adminTarget, node, instanceType string) (*sqsWorkerEnsureOutput, error) {
	form := url.Values{"Action": {"EnsureSQSWorker"}, "NodeId": {node}, "InstanceType": {instanceType}}
	var out sqsWorkerEnsureOutput
	if err := callSpinifexAction(ctx, target, form, &out); err != nil {
		return nil, err
	}
	if out.State != sqsBoundState || out.NodeID != node || out.InstanceID == "" {
		return nil, fmt.Errorf("gateway returned an unexpected worker reply (state %q, node %q)", out.State, out.NodeID)
	}
	return &out, nil
}

// ensureSQSStoreRemote sends EnsureSQSStore and accepts only a bound VM on the
// requested node with the requested instance type.
func ensureSQSStoreRemote(ctx context.Context, target adminTarget, node, instanceType string, sizeGiB int) (*sqsStoreEnsureOutput, error) {
	form := url.Values{
		"Action":        {"EnsureSQSStore"},
		"NodeId":        {node},
		"InstanceType":  {instanceType},
		"VolumeSizeGiB": {strconv.Itoa(sizeGiB)},
	}
	var out sqsStoreEnsureOutput
	if err := callSpinifexAction(ctx, target, form, &out); err != nil {
		return nil, err
	}
	if out.State != sqsBoundState || out.NodeID != node || out.InstanceID == "" || out.InstanceType != instanceType {
		return nil, fmt.Errorf("gateway returned an unexpected store reply (state %q, node %q, type %q)", out.State, out.NodeID, out.InstanceType)
	}
	return &out, nil
}

// callSpinifexAction signs a spinifex query action with the standard AWS
// credential chain and decodes its JSON reply, as GetStorageStatus does.
func callSpinifexAction(ctx context.Context, target adminTarget, form url.Values, out any) error {
	body := form.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(target.endpoint, "/")+"/", strings.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	signer, err := gwsign.NewIMDS(ctx, target.region)
	if err != nil {
		return fmt.Errorf("resolve AWS credentials: %w", err)
	}
	sum := sha256.Sum256([]byte(body))
	if err := signer.Sign(httpReq, hex.EncodeToString(sum[:]), "spinifex", target.region); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	client, err := adminHTTPClient(target.caBundle)
	if err != nil {
		return err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("call %s: %w", target.endpoint, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, adminMaxResponseBytes))
	if err != nil {
		return fmt.Errorf("gateway returned HTTP %d with an unreadable body: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return decodeQueryError(resp.StatusCode, payload)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func renderSQSWorkerEnsure(w io.Writer, out *sqsWorkerEnsureOutput) {
	_, _ = fmt.Fprintf(w, "SQS worker VM bound\n  Node:        %s\n  Region:      %s\n  Instance:    %s\n  Generation:  %d\n",
		out.NodeID, out.Region, out.InstanceID, out.Generation)
	_, _ = fmt.Fprintln(w, "A bound VM is not a ready worker; this does not check SQS or the store.")
}

func renderSQSStoreEnsure(w io.Writer, out *sqsStoreEnsureOutput) {
	_, _ = fmt.Fprintf(w, "SQS store VM bound\n  Node:           %s\n  Region:         %s\n  Instance:       %s\n  Instance type:  %s\n  Address:        %s\n",
		out.NodeID, out.Region, out.InstanceID, out.InstanceType, out.Address)
	_, _ = fmt.Fprintln(w, "A bound VM is not a ready store; the worker checks store readiness itself.")
}
