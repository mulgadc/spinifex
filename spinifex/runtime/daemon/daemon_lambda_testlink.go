//go:build lambdatestlink

package daemon

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
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
// Recovery needs only the owner store, taps and addresses, so it starts even
// when the bridge address, initializer, pin or download trust would not serve.
func (d *Daemon) startLambdaTestLink() {
	if d.mgmtIPAllocator == nil || d.networkPlumber == nil {
		slog.Error("Lambda test link: address or tap cleanup unavailable; not serving or recovering")
		return
	}
	taps, ok := d.networkPlumber.(testlink.Taps)
	if !ok {
		slog.Error("Lambda test link: plumber cannot create owned taps; not serving or recovering")
		return
	}
	workDir := filepath.Join(hostprocess.RuntimeDir(), "lambda-testlink")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		slog.Error("Lambda test link: work directory", "err", err)
		return
	}
	js, err := jetstream.New(d.natsConn)
	if err != nil {
		slog.Error("Lambda test link: JetStream", "err", err)
		return
	}
	grants, err := execution.Open(d.ctx, js)
	if err != nil {
		slog.Error("Lambda test link: execution owner unavailable; not serving or recovering", "err", err)
		return
	}
	httpClient, trustErr := d.lambdaTestLinkHTTP()
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
		HTTP:              httpClient,
	}
	err = runLambdaTestLink(
		func() { go tl.RecoverLoop(d.ctx, testlink.RecoverFirst, testlink.RecoverMax) },
		func() error {
			if d.mgmtBridgeIP == "" {
				return errors.New("management bridge address unavailable")
			}
			if lambdaHardKillSHA256 == "" {
				return errors.New("hard-kill initializer digest not pinned")
			}
			if trustErr != nil {
				return trustErr
			}
			return tl.CheckInitializer()
		},
		func() error { _, err := tl.Serve(d.ctx, d.natsConn, d.node); return err },
	)
	if err != nil {
		slog.Error("Lambda test link: not serving new invocations; recovery continues", "err", err)
		return
	}
	slog.Warn("Lambda test link serving: test-only, not for production; the guest does not verify owner grant authority", "node", d.node)
}

// runLambdaTestLink starts recovery before any serving check, so a failed
// check refuses new invocations without stopping cleanup of earlier ones.
func runLambdaTestLink(startRecovery func(), ready, serve func() error) error {
	startRecovery()
	if err := ready(); err != nil {
		return err
	}
	return serve()
}

// lambdaTestLinkHTTP builds the code-download client trusting the cluster CA.
func (d *Daemon) lambdaTestLinkHTTP() (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("system trust store: %w", err)
	}
	if d.config.NATS.CACert != "" {
		ca, err := os.ReadFile(d.config.NATS.CACert)
		if err != nil || !roots.AppendCertsFromPEM(ca) {
			return nil, errors.New("cluster CA unreadable")
		}
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}, nil
}

var _ testlink.Taps = (*host.OVSPlumber)(nil)
