// Command eks-webhook-cert mints, then reuses, a self-signed serving certificate
// for an in-cluster admission webhook and prints its base64 PEM; the
// implementation is in agents/eks/webhookcert.
//
// Usage:
//
//	eks-webhook-cert -dir /var/lib/spinifex-eks/webhook-certs/<addon> \
//	    -cn <svc>.<ns>.svc -dns <svc>.<ns>.svc,<svc>.<ns>.svc.cluster.local
//
// Output (one line): <ca_b64>\t<tls_crt_b64>\t<tls_key_b64>
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	_ "github.com/mulgadc/bluebottle/pkg/fipsboot"

	"github.com/mulgadc/spinifex/spinifex/agents/eks/webhookcert"
)

func main() {
	cfg := webhookcert.Config{Out: os.Stdout}
	flag.StringVar(&cfg.Dir, "dir", "", "directory to cache tls.crt/tls.key in")
	flag.StringVar(&cfg.CN, "cn", "", "certificate common name")
	flag.StringVar(&cfg.DNS, "dns", "", "comma-separated DNS SANs")
	flag.Parse()

	if err := webhookcert.Run(context.Background(), cfg); err != nil {
		slog.Error("eks-webhook-cert: " + err.Error())
		os.Exit(1)
	}
}
