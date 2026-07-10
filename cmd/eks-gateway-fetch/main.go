// eks-gateway-fetch runs inside the EKS K3s control-plane VM and fetches a
// control-plane resource from the host through the AWS gateway, instead of
// dialing core NATS directly. It is the read-side companion to
// eks-gateway-publish.
//
// It SigV4-signs (service "eks") an HTTPS GET to the gateway, retrying with
// backoff until the gateway returns 2xx or the attempt budget is exhausted (so
// a degraded link surfaces as a non-zero exit), then emits the staged add-ons
// as tab-separated lines the on-VM addon-sync shell agent consumes without a
// JSON parser:
//
//	<addonName>\t<addonVersion>\t<serviceAccountRoleArn>\t<base64(configurationValues)>
//
// The configuration values are base64-encoded so embedded tabs/newlines cannot
// break the line framing.
//
// Usage:
//
//	eks-gateway-fetch -resource addons   # GET /clusters/{cluster}/internal-addons/{accountId}
//	eks-gateway-fetch -resource recovery -instance-id i-… # GET /clusters/{cluster}/internal-recovery/{accountId}/{instanceId}
//
// The recovery resource returns a single epoch\taction\tsnapshot line the on-VM
// k3s-recovery agent applies before k3s starts (etcd cluster-reset / wipe-rejoin).
//
// Flags default to environment variables seeded by cloud-init:
// EKS_GATEWAY_URL, EKS_GATEWAY_CA, EKS_ACCESS_KEY, EKS_SECRET_KEY, EKS_REGION,
// EKS_ACCOUNT_ID, EKS_CLUSTER_NAME, EKS_INSTANCE_ID.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/mulgadc/spinifex/internal/eksgw"

	_ "github.com/mulgadc/spinifex/internal/fipsboot"
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

const (
	maxAttempts = 30
	retryDelay  = 5 * time.Second
)

func main() {
	var (
		gatewayURL  string
		gatewayCA   string
		accessKey   string
		secretKey   string
		region      string
		accountID   string
		clusterName string
		resource    string
		instanceID  string
	)

	flag.StringVar(&gatewayURL, "gateway", os.Getenv("EKS_GATEWAY_URL"), "Gateway URL (e.g. https://10.15.8.1:9999)")
	flag.StringVar(&gatewayCA, "gateway-ca", os.Getenv("EKS_GATEWAY_CA"), "Path to gateway TLS CA PEM (optional; falls back to system trust)")
	flag.StringVar(&accessKey, "access-key", os.Getenv("EKS_ACCESS_KEY"), "AWS access key ID")
	flag.StringVar(&secretKey, "secret-key", os.Getenv("EKS_SECRET_KEY"), "AWS secret access key")
	flag.StringVar(&region, "region", os.Getenv("EKS_REGION"), "AWS region for SigV4 signing")
	flag.StringVar(&accountID, "account-id", os.Getenv("EKS_ACCOUNT_ID"), "Cluster account ID")
	flag.StringVar(&clusterName, "cluster", os.Getenv("EKS_CLUSTER_NAME"), "Cluster name")
	flag.StringVar(&instanceID, "instance-id", os.Getenv("EKS_INSTANCE_ID"), "This member's EC2 instance ID (recovery resource)")
	flag.StringVar(&resource, "resource", "addons", "Resource to fetch: addons | recovery")
	flag.Parse()

	switch {
	case accountID == "":
		fatal("--account-id is required (or set EKS_ACCOUNT_ID)")
	case clusterName == "":
		fatal("--cluster is required (or set EKS_CLUSTER_NAME)")
	case resource != "addons" && resource != "recovery":
		fatal("--resource must be addons or recovery")
	case resource == "recovery" && instanceID == "":
		fatal("--instance-id is required for --resource recovery (or set EKS_INSTANCE_ID)")
	}

	client, err := eksgw.New(gatewayURL, gatewayCA, accessKey, secretKey, region)
	if err != nil {
		fatal(fmt.Sprintf("build gateway client: %v", err))
	}

	var (
		path string
		emit func(io.Writer, []byte) error
	)
	switch resource {
	case "recovery":
		path = "/clusters/" + clusterName + "/internal-recovery/" + accountID + "/" + instanceID
		emit = emitRecoveryTSV
	default:
		path = "/clusters/" + clusterName + "/internal-addons/" + accountID
		emit = emitAddonsTSV
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		body, err := client.Get(path)
		if err != nil {
			lastErr = err
			slog.Warn("eks-gateway-fetch: attempt failed", "resource", resource, "attempt", attempt, "err", err)
			if attempt < maxAttempts {
				time.Sleep(retryDelay)
			}
			continue
		}
		if werr := emit(os.Stdout, body); werr != nil {
			fatal(fmt.Sprintf("render response: %v", werr))
		}
		return
	}
	fatal(fmt.Sprintf("fetch failed after %d attempts: %v", maxAttempts, lastErr))
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

func fatal(msg string) {
	slog.Error("eks-gateway-fetch: " + msg)
	os.Exit(1)
}
