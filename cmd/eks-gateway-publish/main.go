// eks-gateway-publish runs inside the EKS K3s control-plane VM and relays a
// single publication to the host through the AWS gateway, instead of dialing
// core NATS directly.
//
// It reads a JSON payload on stdin, wraps it as
// {accountId, channel, kind, payload}, SigV4-signs (service "eks") an HTTPS
// POST to {gateway}/clusters/{cluster}/internal-publish, and retries with
// backoff until the gateway returns 2xx or the attempt budget is exhausted —
// so a degraded link surfaces as a non-zero exit rather than a silently
// dropped message (the failure mode of fire-and-forget `nats pub`).
//
// Usage:
//
//	echo '{"token":"..."}' | eks-gateway-publish -channel bootstrap -kind k3s-bootstrap-token
//	kubectl ... | eks-gateway-publish -channel state
//
// Flags default to environment variables seeded by cloud-init:
// EKS_GATEWAY_URL, EKS_GATEWAY_CA, EKS_ACCESS_KEY, EKS_SECRET_KEY, EKS_REGION,
// EKS_ACCOUNT_ID, EKS_CLUSTER_NAME.
package main

import (
	"bytes"
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

const (
	// Retry budget: the control plane reaches readiness before this runs, so a
	// failing POST means a degraded link, not a cold start. Bounded so a stuck
	// boot still terminates the OpenRC service.
	maxAttempts = 30
	retryDelay  = 5 * time.Second
)

type publishBody struct {
	AccountID string          `json:"accountId"`
	Channel   string          `json:"channel"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}

func main() {
	var (
		gatewayURL  string
		gatewayCA   string
		accessKey   string
		secretKey   string
		region      string
		accountID   string
		clusterName string
		channel     string
		kind        string
	)

	flag.StringVar(&gatewayURL, "gateway", os.Getenv("EKS_GATEWAY_URL"), "Gateway URL (e.g. https://10.15.8.1:9999)")
	flag.StringVar(&gatewayCA, "gateway-ca", os.Getenv("EKS_GATEWAY_CA"), "Path to gateway TLS CA PEM (optional; falls back to system trust)")
	flag.StringVar(&accessKey, "access-key", os.Getenv("EKS_ACCESS_KEY"), "AWS access key ID")
	flag.StringVar(&secretKey, "secret-key", os.Getenv("EKS_SECRET_KEY"), "AWS secret access key")
	flag.StringVar(&region, "region", os.Getenv("EKS_REGION"), "AWS region for SigV4 signing")
	flag.StringVar(&accountID, "account-id", os.Getenv("EKS_ACCOUNT_ID"), "Cluster account ID")
	flag.StringVar(&clusterName, "cluster", os.Getenv("EKS_CLUSTER_NAME"), "Cluster name")
	flag.StringVar(&channel, "channel", "", "Publish channel: bootstrap|state")
	flag.StringVar(&kind, "kind", "", "Bootstrap subject kind (bootstrap channel only)")
	flag.Parse()

	switch {
	case accountID == "":
		fatal("--account-id is required (or set EKS_ACCOUNT_ID)")
	case clusterName == "":
		fatal("--cluster is required (or set EKS_CLUSTER_NAME)")
	case channel != "bootstrap" && channel != "state":
		fatal("--channel must be bootstrap or state")
	case channel == "bootstrap" && kind == "":
		fatal("--kind is required for the bootstrap channel")
	}

	payload, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatal(fmt.Sprintf("read stdin payload: %v", err))
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		fatal("empty stdin payload")
	}
	if !json.Valid(payload) {
		fatal("stdin payload is not valid JSON")
	}

	body, err := json.Marshal(publishBody{
		AccountID: accountID,
		Channel:   channel,
		Kind:      kind,
		Payload:   json.RawMessage(payload),
	})
	if err != nil {
		fatal(fmt.Sprintf("marshal request body: %v", err))
	}

	client, err := eksgw.New(gatewayURL, gatewayCA, accessKey, secretKey, region)
	if err != nil {
		fatal(fmt.Sprintf("build gateway client: %v", err))
	}
	path := "/clusters/" + clusterName + "/internal-publish"

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if _, err := client.Post(path, body); err != nil {
			lastErr = err
			slog.Warn("eks-gateway-publish: attempt failed",
				"channel", channel, "kind", kind, "attempt", attempt, "err", err)
			if attempt < maxAttempts {
				time.Sleep(retryDelay)
			}
			continue
		}
		slog.Info("eks-gateway-publish: published", "channel", channel, "kind", kind)
		return
	}
	fatal(fmt.Sprintf("publish failed after %d attempts: %v", maxAttempts, lastErr))
}

func fatal(msg string) {
	slog.Error("eks-gateway-publish: " + msg)
	os.Exit(1)
}
