// eks-gateway-fetch runs inside the EKS K3s control-plane VM and fetches a
// control-plane resource from the host through the AWS gateway, writing it to
// stdout as TSV; the implementation is in agents/eks/gatewayfetch.
//
// Usage:
//
//	eks-gateway-fetch -resource addons   # GET /clusters/{cluster}/internal-addons/{accountId}
//	eks-gateway-fetch -resource recovery -instance-id i-… # GET /clusters/{cluster}/internal-recovery/{accountId}/{instanceId}
//
// Flags default to environment variables seeded by cloud-init:
// EKS_GATEWAY_URL, EKS_GATEWAY_CA, EKS_ACCESS_KEY, EKS_SECRET_KEY, EKS_REGION,
// EKS_ACCOUNT_ID, EKS_CLUSTER_NAME, EKS_INSTANCE_ID.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/gatewayfetch"
)

func main() {
	cfg := gatewayfetch.Config{Out: os.Stdout}
	flag.StringVar(&cfg.GatewayURL, "gateway", os.Getenv("EKS_GATEWAY_URL"), "Gateway URL (e.g. https://10.15.8.1:9999)")
	flag.StringVar(&cfg.GatewayCA, "gateway-ca", os.Getenv("EKS_GATEWAY_CA"), "Path to gateway TLS CA PEM (optional; falls back to system trust)")
	flag.StringVar(&cfg.AccessKey, "access-key", os.Getenv("EKS_ACCESS_KEY"), "AWS access key ID")
	flag.StringVar(&cfg.SecretKey, "secret-key", os.Getenv("EKS_SECRET_KEY"), "AWS secret access key")
	flag.StringVar(&cfg.Region, "region", os.Getenv("EKS_REGION"), "AWS region for SigV4 signing")
	flag.StringVar(&cfg.AccountID, "account-id", os.Getenv("EKS_ACCOUNT_ID"), "Cluster account ID")
	flag.StringVar(&cfg.ClusterName, "cluster", os.Getenv("EKS_CLUSTER_NAME"), "Cluster name")
	flag.StringVar(&cfg.InstanceID, "instance-id", os.Getenv("EKS_INSTANCE_ID"), "This member's EC2 instance ID (recovery resource)")
	flag.StringVar(&cfg.Resource, "resource", "addons", "Resource to fetch: addons | recovery")
	flag.Parse()

	if err := gatewayfetch.Run(context.Background(), cfg); err != nil {
		slog.Error("eks-gateway-fetch: " + err.Error())
		os.Exit(1)
	}
}
