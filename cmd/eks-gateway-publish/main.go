// eks-gateway-publish runs inside the EKS K3s control-plane VM and relays a
// single JSON publication from stdin to the host through the AWS gateway; the
// implementation is in agents/eks/gatewaypublish.
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
	"context"
	"flag"
	"log/slog"
	"os"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/gatewaypublish"
)

func main() {
	cfg := gatewaypublish.Config{Payload: os.Stdin}
	flag.StringVar(&cfg.GatewayURL, "gateway", os.Getenv("EKS_GATEWAY_URL"), "Gateway URL (e.g. https://10.15.8.1:9999)")
	flag.StringVar(&cfg.GatewayCA, "gateway-ca", os.Getenv("EKS_GATEWAY_CA"), "Path to gateway TLS CA PEM (optional; falls back to system trust)")
	flag.StringVar(&cfg.AccessKey, "access-key", os.Getenv("EKS_ACCESS_KEY"), "AWS access key ID")
	flag.StringVar(&cfg.SecretKey, "secret-key", os.Getenv("EKS_SECRET_KEY"), "AWS secret access key")
	flag.StringVar(&cfg.Region, "region", os.Getenv("EKS_REGION"), "AWS region for SigV4 signing")
	flag.StringVar(&cfg.AccountID, "account-id", os.Getenv("EKS_ACCOUNT_ID"), "Cluster account ID")
	flag.StringVar(&cfg.ClusterName, "cluster", os.Getenv("EKS_CLUSTER_NAME"), "Cluster name")
	flag.StringVar(&cfg.Channel, "channel", "", "Publish channel: bootstrap|state|addon")
	flag.StringVar(&cfg.Kind, "kind", "", "Bootstrap subject kind (bootstrap channel only)")
	flag.Parse()

	if err := gatewaypublish.Run(context.Background(), cfg); err != nil {
		slog.Error("eks-gateway-publish: " + err.Error())
		os.Exit(1)
	}
}
