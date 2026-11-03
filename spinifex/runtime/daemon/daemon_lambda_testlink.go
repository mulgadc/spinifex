//go:build lambdatestlink

package daemon

import (
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/nats-io/nats.go/jetstream"
)

// lambdaHardKillSHA256 pins the hard-kill initializer; tagged test builds set
// it with -ldflags -X, and an unset pin means the test link does not serve.
var lambdaHardKillSHA256 string

// startLambdaTestLink serves controlled-test Lambda invocations for this node.
// Attempts whose VM stop is unproven keep their tap, address, files and grant,
// and nothing finds them again after a restart.
func (d *Daemon) startLambdaTestLink() {
	if lambdaHardKillSHA256 == "" {
		slog.Error("Lambda test link: hard-kill initializer digest not pinned; not serving")
		return
	}
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
	js, err := jetstream.New(d.natsConn)
	if err != nil {
		slog.Error("Lambda test link: JetStream", "err", err)
		return
	}
	grants, err := execution.Open(d.ctx, js)
	if err != nil {
		slog.Error("Lambda test link: execution owner unavailable; not serving", "err", err)
		return
	}
	tl := &testlink.Daemon{
		Node:     d.node,
		Grants:   grants,
		Taps:     taps,
		IPs:      d.mgmtIPAllocator,
		Bridge:   d.mgmtBridgeName(),
		HostIP:   d.mgmtBridgeIP,
		ImageDir: filepath.Join(microvmDir, "lambda-testlink"),
		Firmware: filepath.Join(microvmDir, vm.MicroVMFirmware),
		WorkDir:  workDir,

		Initializer:       filepath.Join(microvmDir, "lambda-testlink", "hardkill.so"),
		InitializerSHA256: lambdaHardKillSHA256,
		HTTP: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		}},
	}
	if _, err := tl.Serve(d.ctx, d.natsConn, d.node); err != nil {
		slog.Error("Lambda test link: subscribe", "err", err)
		return
	}
	slog.Warn("Lambda test link serving: test-only, guest not bound to the grant, not for production", "node", d.node)
}

var _ testlink.Taps = (*host.OVSPlumber)(nil)
