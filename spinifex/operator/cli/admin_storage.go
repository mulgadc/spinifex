package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/spf13/cobra"
)

// storageStatusTimeout bounds the GetStorageStatus call. The gateway caps its
// own meta node round at a few seconds, so this only guards a hung gateway.
const storageStatusTimeout = 30 * time.Second

var storageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Inspect predastore (S3) storage",
}

var storageStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show predastore meta node raft state and blob nodes",
	Long: `Show the predastore encoding, each meta node's raft state and the blob nodes,
as the web console's Service Metrics page does. The request goes to the AWS
gateway signed with the standard AWS credential chain (e.g. AWS_PROFILE=spinifex).`,
	Args: cobra.NoArgs,
	RunE: runStorageStatus,
}

func init() {
	adminCmd.AddCommand(storageCmd)
	storageCmd.AddCommand(storageStatusCmd)

	storageStatusCmd.Flags().String("endpoint", "", "Gateway endpoint (default: this node's AWS gateway)")
	storageStatusCmd.Flags().String("region", "", "SigV4 region (default: this node's region)")
	storageStatusCmd.Flags().String("ca-bundle", "", "CA certificate (default: this node's CA)")
}

// storageMetaNode is one meta node as GetStorageStatus reports it.
type storageMetaNode struct {
	ID         int    `json:"id"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Healthy    bool   `json:"healthy"`
	State      string `json:"state,omitempty"`
	Leader     string `json:"leader,omitempty"`
	Term       string `json:"term,omitempty"`
	CommitIdx  string `json:"commit_index,omitempty"`
	AppliedIdx string `json:"applied_index,omitempty"`
}

// storageBlobNode is one blob node as GetStorageStatus reports it.
type storageBlobNode struct {
	ID   int    `json:"id"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

// storageStatus is the GetStorageStatus response body.
type storageStatus struct {
	Encoding struct {
		Type         string `json:"type"`
		DataShards   int    `json:"data_shards"`
		ParityShards int    `json:"parity_shards"`
	} `json:"encoding"`
	MetaNodes []storageMetaNode `json:"meta_nodes"`
	BlobNodes []storageBlobNode `json:"blob_nodes"`
}

func runStorageStatus(cmd *cobra.Command, _ []string) error {
	target, err := resolveAdminTarget(cmd)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), storageStatusTimeout)
	defer cancel()

	status, err := getStorageStatusRemote(ctx, target)
	if err != nil {
		return err
	}
	return renderStorageStatus(os.Stdout, status)
}

// getStorageStatusRemote signs and sends the spinifex GetStorageStatus action,
// the same query-protocol request the web console makes.
func getStorageStatusRemote(ctx context.Context, target adminTarget) (*storageStatus, error) {
	body := "Action=GetStorageStatus"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(target.endpoint, "/")+"/", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")

	signer, err := gwsign.NewIMDS(ctx, target.region)
	if err != nil {
		return nil, fmt.Errorf("resolve AWS credentials: %w", err)
	}
	sum := sha256.Sum256([]byte(body))
	if err := signer.Sign(httpReq, hex.EncodeToString(sum[:]), "spinifex", target.region); err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	client, err := adminHTTPClient(target.caBundle)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", target.endpoint, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, adminMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("gateway returned HTTP %d with an unreadable body: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decodeQueryError(resp.StatusCode, payload)
	}

	var out storageStatus
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

// decodeQueryError reads the EC2-shaped XML error envelope the gateway returns
// for spinifex actions. Any other body is reported verbatim.
func decodeQueryError(statusCode int, payload []byte) error {
	var body struct {
		Errors struct {
			Error struct {
				Code    string `xml:"Code"`
				Message string `xml:"Message"`
			} `xml:"Error"`
		} `xml:"Errors"`
		RequestID string `xml:"RequestID"`
	}
	if err := xml.Unmarshal(payload, &body); err != nil || body.Errors.Error.Code == "" {
		return fmt.Errorf("gateway returned HTTP %d: %s", statusCode, payload)
	}
	return &adminError{
		Code:       body.Errors.Error.Code,
		Message:    body.Errors.Error.Message,
		RequestID:  body.RequestID,
		StatusCode: statusCode,
	}
}

// storageClusterHealth matches the console's badge: every meta node must
// answer, and every node reporting raft state must name the same leader.
func storageClusterHealth(nodes []storageMetaNode) string {
	if len(nodes) == 0 {
		return "Unknown"
	}
	leaders := map[string]struct{}{}
	for _, n := range nodes {
		if !n.Healthy {
			return "Degraded"
		}
		if n.State != "" {
			leaders[n.Leader] = struct{}{}
		}
	}
	if _, empty := leaders[""]; empty || len(leaders) > 1 {
		return "Degraded"
	}
	return "Healthy"
}

func renderStorageStatus(w io.Writer, status *storageStatus) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	encoding := "-"
	if status.Encoding.Type != "" {
		encoding = fmt.Sprintf("%s RS(%d,%d)", status.Encoding.Type, status.Encoding.DataShards, status.Encoding.ParityShards)
	}
	fmt.Fprintf(tw, "Encoding:\t%s\n", encoding)
	fmt.Fprintf(tw, "Meta Nodes:\t%d\n", len(status.MetaNodes))
	fmt.Fprintf(tw, "Blob Nodes:\t%d\n", len(status.BlobNodes))
	fmt.Fprintf(tw, "Cluster Health:\t%s\n", storageClusterHealth(status.MetaNodes))
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w, "\nMeta Nodes")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tHOST\tPORT\tHEALTH\tSTATE\tLEADER\tTERM\tCOMMIT INDEX\tAPPLIED INDEX")
	for _, n := range status.MetaNodes {
		health := "Unreachable"
		if n.Healthy {
			health = "Healthy"
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.ID, n.Host, n.Port, health, orDash(n.State), orDash(n.Leader), orDash(n.Term), orDash(n.CommitIdx), orDash(n.AppliedIdx))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w, "\nBlob Nodes")
	if len(status.BlobNodes) == 0 {
		_, err := fmt.Fprintln(w, "No blob nodes configured.")
		return err
	}
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tHOST\tPORT")
	for _, n := range status.BlobNodes {
		fmt.Fprintf(tw, "%d\t%s\t%s\n", n.ID, n.Host, strconv.Itoa(n.Port))
	}
	return tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
