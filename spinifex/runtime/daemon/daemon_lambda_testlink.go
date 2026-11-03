//go:build lambdatestlink

package daemon

import (
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
)

// startLambdaTestLink serves controlled-test Lambda invocations for this node.
// Attempts whose VM stop is unproven keep their tap, address and files, and
// nothing finds them again after a restart.
func (d *Daemon) startLambdaTestLink() {
	if d.mgmtIPAllocator == nil || d.mgmtBridgeIP == "" || d.networkPlumber == nil {
		slog.Error("Lambda test link: management bridge unavailable; not serving")
		return
	}
	taps, ok := d.networkPlumber.(testlink.Taps)
	if !ok {
		slog.Error("Lambda test link: plumber cannot create owned taps; not serving")
		return
	}
	workDir := filepath.Join(hostprocess.RuntimeDir(), "lambda-testlink")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		slog.Error("Lambda test link: work directory", "err", err)
		return
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		slog.Error("Lambda test link: system trust store", "err", err)
		return
	}
	if d.config.NATS.CACert != "" {
		ca, err := os.ReadFile(d.config.NATS.CACert)
		if err != nil || !roots.AppendCertsFromPEM(ca) {
			slog.Error("Lambda test link: cluster CA unreadable; not serving", "err", err)
			return
		}
	}
	tl := &testlink.Daemon{
		Taps:     taps,
		IPs:      d.mgmtIPAllocator,
		Bridge:   d.mgmtBridgeName(),
		HostIP:   d.mgmtBridgeIP,
		ImageDir: filepath.Join(microvmDir, "lambda-testlink"),
		Firmware: filepath.Join(microvmDir, vm.MicroVMFirmware),
		WorkDir:  workDir,
		HTTP: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		}},
	}
	if _, err := tl.Serve(d.ctx, d.natsConn, d.node); err != nil {
		slog.Error("Lambda test link: subscribe", "err", err)
		return
	}
	slog.Warn("Lambda test link serving: test-only, no grant, not for production", "node", d.node)
}

var _ testlink.Taps = (*host.OVSPlumber)(nil)
