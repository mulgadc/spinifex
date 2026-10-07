// Command eks-token-webhook is the K8s TokenReview webhook authenticator baked
// into the eks-server AMI; the implementation is in agents/eks/tokenwebhook.
// It reads its configuration from the first-boot env cloud-init seeds onto the
// VM and binds loopback only.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/tokenwebhook"
)

func loadConfig() (tokenwebhook.Config, error) {
	c := tokenwebhook.Config{
		Addr:        flagAddr,
		GatewayURL:  os.Getenv("EKS_GATEWAY_URL"),
		GatewayCA:   os.Getenv("EKS_GATEWAY_CA"),
		AccessKey:   os.Getenv("EKS_ACCESS_KEY"),
		SecretKey:   os.Getenv("EKS_SECRET_KEY"),
		Region:      os.Getenv("EKS_REGION"),
		AccountID:   os.Getenv("EKS_ACCOUNT_ID"),
		ClusterName: os.Getenv("EKS_CLUSTER_NAME"),
		CertPath:    envOr("EKS_WEBHOOK_CERT", "/etc/spinifex-eks/token-webhook.crt"),
		KeyPath:     envOr("EKS_WEBHOOK_KEY", "/etc/spinifex-eks/token-webhook.key"),
		Kubeconfig:  envOr("EKS_WEBHOOK_KUBECONFIG", "/etc/spinifex-eks/token-webhook.kubeconfig"),
	}
	// Static SigV4 creds are optional: when absent, eksgw.New signs with the
	// AWS SDK chain (IMDS instance-role creds), the same path the CP VM's
	// sibling helpers use. The CP VM launches on an instance profile, so
	// buildK3sUserData omits EKS_ACCESS_KEY/EKS_SECRET_KEY by design.
	switch {
	case c.GatewayURL == "":
		return c, errors.New("EKS_GATEWAY_URL not set")
	case c.AccountID == "":
		return c, errors.New("EKS_ACCOUNT_ID not set")
	case c.ClusterName == "":
		return c, errors.New("EKS_CLUSTER_NAME not set")
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

var flagAddr string

func main() {
	flag.StringVar(&flagAddr, "addr", "127.0.0.1:8443", "listen address (loopback only)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("eks-token-webhook config error", "err", err)
		os.Exit(1)
	}

	if err := tokenwebhook.Run(context.Background(), cfg); err != nil {
		slog.Error("eks-token-webhook fatal", "err", err)
		os.Exit(1)
	}
}
