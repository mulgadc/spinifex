// Command eks-konnectivity-cert mints, then reuses, a K3s-CA-signed serving
// certificate for the on-node konnectivity-server; the implementation is in
// agents/eks/konnectivitycert.
//
// Usage:
//
//	eks-konnectivity-cert -dir /var/lib/spinifex-eks/konnectivity \
//	    -ca-cert /var/lib/rancher/k3s/server/tls/server-ca.crt \
//	    -ca-key  /var/lib/rancher/k3s/server/tls/server-ca.key \
//	    -cn konnectivity-server -sans 10.32.100.4,203.0.113.9,eks-toc.example
//
// Writes <dir>/tls.crt and <dir>/tls.key; prints the two paths (tab-separated).
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/konnectivitycert"
)

func main() {
	cfg := konnectivitycert.Config{Out: os.Stdout}
	flag.StringVar(&cfg.Dir, "dir", "", "directory to cache tls.crt/tls.key in")
	flag.StringVar(&cfg.CACert, "ca-cert", "", "PEM path of the signing (K3s server) CA cert")
	flag.StringVar(&cfg.CAKey, "ca-key", "", "PEM path of the signing CA private key")
	flag.StringVar(&cfg.CN, "cn", "konnectivity-server", "certificate common name")
	flag.StringVar(&cfg.SANs, "sans", "", "comma-separated SANs (IP or DNS)")
	flag.Parse()

	if err := konnectivitycert.Run(context.Background(), cfg); err != nil {
		slog.Error("eks-konnectivity-cert: " + err.Error())
		os.Exit(1)
	}
}
