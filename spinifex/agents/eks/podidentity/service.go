package podidentity

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

const (
	// DefaultAddr is the loopback listener; 8443 belongs to eks-token-webhook.
	DefaultAddr = "127.0.0.1:8444"
	// MutatePath is the URL path the API server calls.
	MutatePath = "/mutate"

	defaultStateDir   = "/var/lib/spinifex-eks/pod-identity"
	defaultTLSDir     = "/var/lib/rancher/k3s/server/tls"
	defaultAPIServer  = "https://127.0.0.1:6443"
	defaultSystemRoot = "/etc/ssl/certs/ca-certificates.crt"
	defaultManifests  = "/var/lib/rancher/k3s/server/manifests"

	manifestFile = "eks-pod-identity-webhook.yaml"
)

// ServiceConfig is everything the webhook process needs.
type ServiceConfig struct {
	Addr          string
	Dir           string
	TLSDir        string
	APIServer     string
	Region        string
	STSEndpoint   string
	GatewayCAFile string
	SystemRoots   string
	ManifestDir   string
	Resync        time.Duration
}

// ParseServiceConfig reads flags, defaulting region, STS endpoint and gateway
// CA from the environment the node already has (the cloud-init first-boot env
// that mulga-eks-addon-sync renders the managed add-ons from).
func ParseServiceConfig(args []string, getenv func(string) string, errOut io.Writer) (ServiceConfig, error) {
	var c ServiceConfig
	fs := flag.NewFlagSet("eks-pod-identity-webhook", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&c.Addr, "addr", DefaultAddr, "loopback listen address")
	fs.StringVar(&c.Dir, "dir", defaultStateDir, "directory holding the webhook's certificates")
	fs.StringVar(&c.TLSDir, "k3s-tls-dir", defaultTLSDir, "k3s server tls directory")
	fs.StringVar(&c.APIServer, "apiserver", defaultAPIServer, "kube-apiserver URL")
	fs.StringVar(&c.Region, "region", getenv("EKS_REGION"), "region injected as AWS_REGION")
	fs.StringVar(&c.STSEndpoint, "sts-endpoint", getenv("EKS_ADDON_GATEWAY_URL"), "endpoint injected as AWS_ENDPOINT_URL_STS")
	fs.StringVar(&c.GatewayCAFile, "gateway-ca", getenv("EKS_GATEWAY_CA"), "gateway CA PEM added to the published bundle")
	fs.StringVar(&c.SystemRoots, "system-roots", defaultSystemRoot, "system root certificates PEM")
	fs.StringVar(&c.ManifestDir, "manifest-dir", defaultManifests, "k3s auto-deploy manifests directory")
	fs.DurationVar(&c.Resync, "resync", 5*time.Minute, "how often every namespace's CA ConfigMap is re-checked")
	if err := fs.Parse(args); err != nil {
		return ServiceConfig{}, err
	}
	return c, c.Validate()
}

// Validate rejects a configuration that would expose the webhook beyond the
// node or run it with nothing to inject.
func (c ServiceConfig) Validate() error {
	host, _, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("addr %q: %w", c.Addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("addr %q must be a loopback IP: only the local kube-apiserver may call the webhook", c.Addr)
	}
	if c.Region == "" || c.STSEndpoint == "" {
		return errors.New("region and sts endpoint are required (EKS_REGION, EKS_ADDON_GATEWAY_URL)")
	}
	return nil
}

// CertConfig returns the certificate settings that follow from the k3s tls directory.
func (c ServiceConfig) CertConfig() CertConfig {
	return CertConfig{
		Dir:          c.Dir,
		ServerCACert: c.TLSDir + "/server-ca.crt", ServerCAKey: c.TLSDir + "/server-ca.key",
		ClientCACert: c.TLSDir + "/client-ca.crt", ClientCAKey: c.TLSDir + "/client-ca.key",
	}
}

// Run serves the webhook on the loopback address and keeps the CA ConfigMaps
// published until ctx ends.
func Run(ctx context.Context, c ServiceConfig, log *slog.Logger) error {
	if err := c.Validate(); err != nil {
		return err
	}
	certs, err := EnsureCerts(c.CertConfig())
	if err != nil {
		return err
	}
	bundle, err := c.caBundle()
	if err != nil {
		return err
	}
	if err := c.stageManifests(); err != nil {
		return err
	}
	kube, err := NewKubeClient(c.APIServer, c.TLSDir+"/server-ca.crt", certs.ClientCert, certs.ClientKey)
	if err != nil {
		return err
	}
	pair, err := tls.LoadX509KeyPair(certs.ServingCert, certs.ServingKey)
	if err != nil {
		return fmt.Errorf("load serving certificate: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle(MutatePath, NewMutator(Config{Region: c.Region, STSEndpoint: c.STSEndpoint}, kube, log))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	srv := &http.Server{
		Handler:           mux,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	ln, err := net.Listen("tcp", c.Addr)
	if err != nil {
		return err
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	go func() {
		if err := NewPublisher(PublisherConfig{Bundle: bundle}, kube, kube, log).Run(ctx, c.Resync); err != nil {
			log.Error("ca bundle publisher stopped", "error", err)
		}
	}()
	log.Info("pod identity webhook listening", "addr", c.Addr)

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	case err := <-errc:
		return err
	}
}

// caBundle is the system roots plus the gateway CA, so a client that trusts only
// this bundle still reaches public endpoints as well as the gateway.
func (c ServiceConfig) caBundle() ([]byte, error) {
	roots, err := os.ReadFile(c.SystemRoots)
	if err != nil {
		return nil, fmt.Errorf("read system roots: %w", err)
	}
	var gw []byte
	if c.GatewayCAFile != "" {
		if gw, err = os.ReadFile(c.GatewayCAFile); err != nil {
			return nil, fmt.Errorf("read gateway ca: %w", err)
		}
	}
	b := BuildCABundle(roots, gw)
	if len(b) == 0 {
		return nil, errors.New("ca bundle is empty")
	}
	return b, nil
}

// stageManifests writes the RBAC and webhook configuration into the k3s
// auto-deploy directory. Every server renders the same bytes (the CA is shared),
// and the file is only replaced when its content changes, so k3s re-applies it
// only on a real change.
func (c ServiceConfig) stageManifests() error {
	ca, err := os.ReadFile(c.TLSDir + "/server-ca.crt")
	if err != nil {
		return fmt.Errorf("read server ca: %w", err)
	}
	body, err := RenderManifests(WebhookConfig{Name: WebhookName, URL: "https://" + c.Addr + MutatePath, CA: ca})
	if err != nil {
		return err
	}
	path := c.ManifestDir + "/" + manifestFile
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, body) {
		return nil
	}
	return writeAtomic(path, body, 0o600)
}
