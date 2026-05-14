// lb-agent runs inside load balancer VMs and manages HAProxy configuration.
//
// It polls the AWS gateway for config updates and reports health via heartbeats.
// All communication uses SigV4-signed HTTP requests.
//
// Usage:
//
//	lb-agent --lb-id=lb-xxxxx --gateway=https://192.168.1.33:9999 --access-key=AKIA... --secret-key=...
//
// Flags default to environment variables set by cloud-init:
// LB_LB_ID, LB_GATEWAY_URL, LB_ACCESS_KEY, LB_SECRET_KEY, LB_REGION.
package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mulgadc/spinifex/spinifex/lbagent"

	_ "github.com/mulgadc/spinifex/internal/fipsboot"
)

func main() {
	var (
		lbID       string
		gatewayURL string
		accessKey  string
		secretKey  string
		region     string
	)

	hostname, _ := os.Hostname()

	flag.StringVar(&lbID, "lb-id", envOrDefault("LB_LB_ID", hostname), "Load balancer ID")
	flag.StringVar(&gatewayURL, "gateway", os.Getenv("LB_GATEWAY_URL"), "Gateway URL (e.g. https://192.168.1.33:9999)")
	flag.StringVar(&accessKey, "access-key", os.Getenv("LB_ACCESS_KEY"), "AWS access key ID")
	flag.StringVar(&secretKey, "secret-key", os.Getenv("LB_SECRET_KEY"), "AWS secret access key")
	flag.StringVar(&region, "region", envOrDefault("LB_REGION", "us-east-1"), "AWS region for SigV4 signing")
	flag.Parse()

	if lbID == "" {
		slog.Error("--lb-id is required (or set LB_LB_ID)")
		os.Exit(1)
	}
	if gatewayURL == "" {
		slog.Error("--gateway is required (or set LB_GATEWAY_URL)")
		os.Exit(1)
	}
	if accessKey == "" || secretKey == "" {
		slog.Error("--access-key and --secret-key are required (or set LB_ACCESS_KEY / LB_SECRET_KEY)")
		os.Exit(1)
	}

	slog.Info("Starting LB agent", "lbId", lbID, "gateway", gatewayURL)

	agent, err := lbagent.New(lbID, gatewayURL, accessKey, secretKey, region)
	if err != nil {
		slog.Error("Failed to create agent", "err", err)
		os.Exit(1)
	}

	// Start poll loop in a goroutine — it blocks until Stop is called.
	errCh := make(chan error, 1)
	go func() {
		if err := agent.Start(); err != nil {
			errCh <- err
		}
	}()

	// Wait for shutdown signal or fatal error.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		slog.Info("Received signal, shutting down", "signal", sig)
	case err := <-errCh:
		slog.Error("Agent error", "err", err)
	}

	agent.Stop()
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
