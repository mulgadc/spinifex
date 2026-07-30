package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
)

var getDHCPLeasesCmd = &cobra.Command{
	Use:     "dhcp-leases",
	Aliases: []string{"leases"},
	Short:   "Display upstream DHCP leases held for external pools",
	Long: `Display the upstream DHCP leases vpcd holds for source="dhcp" external pools,
with the resource each was taken for and whether that resource still exists.

An OWNER of "gone" is a leaked address: nothing renews it on purpose and nothing
will release it. "unknown" means the owning service could not be reached.`,
	Run: runGetDHCPLeases,
}

func init() {
	getCmd.AddCommand(getDHCPLeasesCmd)
	getDHCPLeasesCmd.Flags().Bool("orphans", false, "Show only leases whose owning resource is gone")
	getDHCPLeasesCmd.Flags().Bool("skip-owner-check", false, "Skip the owner lookup (faster; OWNER reads as unchecked)")
}

func runGetDHCPLeases(cmd *cobra.Command, args []string) {
	_, nc, err := loadConfigAndConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer nc.Close()

	orphansOnly, _ := cmd.Flags().GetBool("orphans")
	skipOwnerCheck, _ := cmd.Flags().GetBool("skip-owner-check")
	timeout, _ := cmd.Flags().GetDuration("timeout")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	entries, err := listDHCPLeases(ctx, nc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if len(entries) == 0 {
		pterm.Info.Println("No DHCP leases held (no source=\"dhcp\" external pool, or none taken yet)")
		return
	}

	tableData := pterm.TableData{
		{"CLIENT ID", "AZ", "PURPOSE", "VPC", "IP", "MAC", "NODE", "EXPIRES", "OWNER"},
	}
	shown := 0
	for _, l := range entries {
		owner := "unchecked"
		if !skipOwnerCheck {
			owner = dhcpLeaseOwnerStatus(ctx, nc, l.entry, timeout)
		}
		if orphansOnly && owner != dhcp.OwnerStatusGone {
			continue
		}
		shown++
		tableData = append(tableData, []string{
			l.entry.Lease.ClientID,
			l.az,
			l.entry.Purpose,
			l.entry.VPCID,
			l.entry.Lease.IP.String(),
			l.entry.Lease.HWAddr.String(),
			leaseNodeLabel(l.entry),
			formatLeaseExpiry(l.entry),
			owner,
		})
	}

	if shown == 0 {
		pterm.Info.Printfln("No orphaned leases among %d held", len(entries))
		return
	}
	if err := pterm.DefaultTable.WithHasHeader().WithData(tableData).Render(); err != nil {
		fmt.Fprintf(os.Stderr, "Error rendering table: %v\n", err)
		os.Exit(1)
	}
}

// azEntry pairs a lease with the AZ whose bucket held it, since the client-id
// alone does not say which vpcd owns it.
type azEntry struct {
	az    string
	entry dhcp.Entry
}

// listDHCPLeases reads every per-AZ lease bucket. Buckets are enumerated rather
// than derived from config so an AZ that no longer appears in the local config
// still shows the addresses it is holding.
func listDHCPLeases(ctx context.Context, nc *nats.Conn) ([]azEntry, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	buckets, err := kvutil.BucketNames(ctx, js)
	if err != nil {
		return nil, fmt.Errorf("enumerate KV buckets: %w", err)
	}

	var out []azEntry
	for _, bucket := range buckets {
		if !strings.HasPrefix(bucket, dhcp.KVBucketPrefix) {
			continue
		}
		az := strings.TrimPrefix(strings.TrimPrefix(bucket, dhcp.KVBucketPrefix), "-")
		kv, err := js.KeyValue(ctx, bucket)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", bucket, err)
		}
		entries, err := dhcp.NewStoreWithKV(kv, az).List(ctx)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", bucket, err)
		}
		for _, e := range entries {
			if e.Lease == nil {
				continue
			}
			out = append(out, azEntry{az: az, entry: e})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].az != out[j].az {
			return out[i].az < out[j].az
		}
		return out[i].entry.Lease.ClientID < out[j].entry.Lease.ClientID
	})
	return out, nil
}

// dhcpLeaseOwnerStatus asks the daemon whether the lease's resource survives.
// Reported as unknown on any failure, matching what the reaper acts on.
func dhcpLeaseOwnerStatus(ctx context.Context, nc *nats.Conn, e dhcp.Entry, timeout time.Duration) string {
	payload, err := json.Marshal(dhcp.OwnerCheckRequest{
		ClientID: e.Lease.ClientID,
		Purpose:  e.Purpose,
		VPCID:    e.VPCID,
	})
	if err != nil {
		return dhcp.OwnerStatusUnknown
	}
	reqCtx, cancel := context.WithTimeout(ctx, max(timeout, time.Second))
	defer cancel()
	msg, err := nc.RequestWithContext(reqCtx, dhcp.TopicOwnerCheck, payload)
	if err != nil {
		return dhcp.OwnerStatusUnknown
	}
	var reply dhcp.OwnerCheckReply
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return dhcp.OwnerStatusUnknown
	}
	return dhcp.ParseOwnerStatus(reply.Status).String()
}

// leaseNodeLabel recovers the node that took the lease from option 60, which
// carries "<vendor class>/<node>".
func leaseNodeLabel(e dhcp.Entry) string {
	_, node, found := strings.Cut(e.Lease.VendorClass, "/")
	if !found {
		return "-"
	}
	return node
}

// formatLeaseExpiry renders the remaining lease time, flagging one the server
// has already aged out.
func formatLeaseExpiry(e dhcp.Entry) string {
	expiry := e.Lease.ExpiresAt()
	if expiry.IsZero() {
		return "-"
	}
	remaining := time.Until(expiry).Round(time.Second)
	if remaining <= 0 {
		return "expired"
	}
	return remaining.String()
}
