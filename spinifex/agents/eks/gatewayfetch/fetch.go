// Package gatewayfetch implements eks-gateway-fetch, which runs inside the EKS
// K3s control-plane VM and fetches a control-plane resource from the host
// through the AWS gateway, instead of dialing core NATS directly. It is the
// read-side companion to eks-gateway-publish.
//
// It SigV4-signs (service "eks") an HTTPS GET to the gateway, retrying with
// backoff until the gateway returns 2xx or the attempt budget is exhausted (so
// a degraded link surfaces as an error), then emits the staged add-ons as
// tab-separated lines the on-VM addon-sync shell agent consumes without a JSON
// parser:
//
//	<addonName>\t<addonVersion>\t<serviceAccountRoleArn>\t<base64(configurationValues)>
//
// The configuration values are base64-encoded so embedded tabs/newlines cannot
// break the line framing.
//
// The recovery resource returns a single epoch\taction\tsnapshot line the on-VM
// k3s-recovery agent applies before k3s starts (etcd cluster-reset / wipe-rejoin).
package gatewayfetch

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/internal/eksgw"
)

// stagedAddon mirrors the gateway's StagedAddonManifest wire shape; duplicated
// here to keep this tiny VM binary free of the handler package's dependency
// graph.
type stagedAddon struct {
	AddonName             string `json:"addonName"`
	AddonVersion          string `json:"addonVersion"`
	ServiceAccountRoleArn string `json:"serviceAccountRoleArn"`
	ConfigurationValues   string `json:"configurationValues"`
}

type internalAddonsResponse struct {
	Addons []stagedAddon `json:"addons"`
}

const maxAttempts = 30

var retryDelay = 5 * time.Second

// Config selects the resource to fetch and the gateway it is fetched through.
type Config struct {
	GatewayURL  string
	GatewayCA   string
	AccessKey   string
	SecretKey   string
	Region      string
	AccountID   string
	ClusterName string
	// Resource is "addons" or "recovery".
	Resource   string
	InstanceID string
	// Out receives the rendered TSV.
	Out io.Writer
}

// Run validates cfg, fetches the resource with retries and renders it to
// cfg.Out. It stops early if ctx is cancelled between attempts.
func Run(ctx context.Context, cfg Config) error {
	switch {
	case cfg.AccountID == "":
		return errors.New("--account-id is required (or set EKS_ACCOUNT_ID)")
	case cfg.ClusterName == "":
		return errors.New("--cluster is required (or set EKS_CLUSTER_NAME)")
	case cfg.Resource != "addons" && cfg.Resource != "recovery":
		return errors.New("--resource must be addons or recovery")
	case cfg.Resource == "recovery" && cfg.InstanceID == "":
		return errors.New("--instance-id is required for --resource recovery (or set EKS_INSTANCE_ID)")
	}

	client, err := eksgw.New(cfg.GatewayURL, cfg.GatewayCA, cfg.AccessKey, cfg.SecretKey, cfg.Region)
	if err != nil {
		return fmt.Errorf("build gateway client: %w", err)
	}

	var (
		path string
		emit func(io.Writer, []byte) error
	)
	switch cfg.Resource {
	case "recovery":
		path = "/clusters/" + cfg.ClusterName + "/internal-recovery/" + cfg.AccountID + "/" + cfg.InstanceID
		emit = emitRecoveryTSV
	default:
		path = "/clusters/" + cfg.ClusterName + "/internal-addons/" + cfg.AccountID
		emit = emitAddonsTSV
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		body, err := client.Get(path)
		if err != nil {
			lastErr = err
			slog.Warn("eks-gateway-fetch: attempt failed", "resource", cfg.Resource, "attempt", attempt, "err", err)
			if attempt < maxAttempts {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(retryDelay):
				}
			}
			continue
		}
		if werr := emit(cfg.Out, body); werr != nil {
			return fmt.Errorf("render response: %w", werr)
		}
		return nil
	}
	return fmt.Errorf("fetch failed after %d attempts: %w", maxAttempts, lastErr)
}

// emitAddonsTSV decodes the internal-addons response and writes one TSV line per
// staged add-on to w. An empty add-on set writes nothing (the agent then GCs
// every locally-rendered manifest), which is the correct steady state for a
// cluster with no managed add-ons.
func emitAddonsTSV(out io.Writer, body []byte) error {
	var resp internalAddonsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("unmarshal internal-addons response: %w", err)
	}
	w := bufio.NewWriter(out)
	for _, a := range resp.Addons {
		cfg := base64.StdEncoding.EncodeToString([]byte(a.ConfigurationValues))
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.AddonName, a.AddonVersion, a.ServiceAccountRoleArn, cfg); err != nil {
			return err
		}
	}
	return w.Flush()
}

// recoveryDirective mirrors the gateway's RecoveryDirective wire shape; duplicated
// here to keep this tiny VM binary free of the handler package's dependency graph.
type recoveryDirective struct {
	Epoch            int64  `json:"epoch"`
	Action           string `json:"action"`
	Snapshot         string `json:"snapshot"`
	SnapshotRequired bool   `json:"snapshotRequired"`
}

type internalRecoveryResponse struct {
	Directive recoveryDirective `json:"directive"`
}

// emitRecoveryTSV decodes the recovery directive and writes a single
// epoch\taction\tsnapshot\tsnapshotRequired line the on-VM k3s-recovery agent
// consumes without a JSON parser. An unset action defaults to "none" (steady
// state); snapshotRequired is 1 when the snapshot MUST restore (fresh DR seed
// aborts boot on fetch failure) else 0.
func emitRecoveryTSV(out io.Writer, body []byte) error {
	var resp internalRecoveryResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("unmarshal internal-recovery response: %w", err)
	}
	d := resp.Directive
	if d.Action == "" {
		d.Action = "none"
	}
	required := "0"
	if d.SnapshotRequired {
		required = "1"
	}
	if _, err := fmt.Fprintf(out, "%d\t%s\t%s\t%s\n", d.Epoch, d.Action, d.Snapshot, required); err != nil {
		return err
	}
	return nil
}
