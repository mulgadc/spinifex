// eks-token-webhook is the K8s TokenReview webhook authenticator baked into
// the eks-server AMI. It decodes the SigV4-presigned GetCallerIdentity URL
// produced by `aws eks get-token`, enforces the X-K8s-Aws-Id cross-cluster
// pin, calls Mulga STS over NATS, and resolves the principal to a TokenReview
// response via the cluster's AccessEntry KV.
//
// This is a placeholder build that returns 503 Service Unavailable on every
// path. It exists so the eks-server AMI build succeeds (INSTALL_BINARIES is
// satisfied) and the K3s server VM boots cleanly. Real implementation lands
// with bead mulga-cs-eks-6c.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8443", "listen address")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"eks-token-webhook stub: real implementation not yet deployed"}`, http.StatusServiceUnavailable)
	})

	slog.Info("eks-token-webhook stub starting", "addr", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		slog.Error("listen failed", "err", err)
		os.Exit(1)
	}
}
