// Package gatewaypublish implements eks-gateway-publish, which runs inside the
// EKS K3s control-plane VM and relays a single publication to the host through
// the AWS gateway, instead of dialing core NATS directly.
//
// It reads a JSON payload, wraps it as {accountId, channel, kind, payload},
// SigV4-signs (service "eks") an HTTPS POST to
// {gateway}/clusters/{cluster}/internal-publish, and retries with backoff until
// the gateway returns 2xx or the attempt budget is exhausted — so a degraded
// link surfaces as an error rather than a silently dropped message (the failure
// mode of fire-and-forget `nats pub`).
package gatewaypublish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/internal/eksgw"
)

// Retry budget: the control plane reaches readiness before this runs, so a
// failing POST means a degraded link, not a cold start. Bounded so a stuck
// boot still terminates the OpenRC service.
const maxAttempts = 30

var retryDelay = 5 * time.Second

// Config is one publication and the gateway it is sent through.
type Config struct {
	GatewayURL  string
	GatewayCA   string
	AccessKey   string
	SecretKey   string
	Region      string
	AccountID   string
	ClusterName string
	Channel     string
	Kind        string
	// Payload supplies the JSON document to publish.
	Payload io.Reader
}

type publishBody struct {
	AccountID string          `json:"accountId"`
	Channel   string          `json:"channel"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}

// Run validates cfg, reads the payload and publishes it, retrying until the
// gateway accepts it, the attempt budget is spent, or ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	switch {
	case cfg.AccountID == "":
		return errors.New("--account-id is required (or set EKS_ACCOUNT_ID)")
	case cfg.ClusterName == "":
		return errors.New("--cluster is required (or set EKS_CLUSTER_NAME)")
	case cfg.Channel != "bootstrap" && cfg.Channel != "state" && cfg.Channel != "addon":
		return errors.New("--channel must be bootstrap, state or addon")
	case cfg.Channel == "bootstrap" && cfg.Kind == "":
		return errors.New("--kind is required for the bootstrap channel")
	}

	payload, err := io.ReadAll(cfg.Payload)
	if err != nil {
		return fmt.Errorf("read stdin payload: %w", err)
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return errors.New("empty stdin payload")
	}
	if !json.Valid(payload) {
		return errors.New("stdin payload is not valid JSON")
	}

	body, err := json.Marshal(publishBody{
		AccountID: cfg.AccountID,
		Channel:   cfg.Channel,
		Kind:      cfg.Kind,
		Payload:   json.RawMessage(payload),
	})
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	client, err := eksgw.New(cfg.GatewayURL, cfg.GatewayCA, cfg.AccessKey, cfg.SecretKey, cfg.Region)
	if err != nil {
		return fmt.Errorf("build gateway client: %w", err)
	}
	path := "/clusters/" + cfg.ClusterName + "/internal-publish"

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if _, err := client.Post(path, body); err != nil {
			lastErr = err
			slog.Warn("eks-gateway-publish: attempt failed",
				"channel", cfg.Channel, "kind", cfg.Kind, "attempt", attempt, "err", err)
			if attempt < maxAttempts {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(retryDelay):
				}
			}
			continue
		}
		slog.Info("eks-gateway-publish: published", "channel", cfg.Channel, "kind", cfg.Kind)
		return nil
	}
	return fmt.Errorf("publish failed after %d attempts: %w", maxAttempts, lastErr)
}
