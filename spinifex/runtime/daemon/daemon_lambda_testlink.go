//go:build lambdatestlink

package daemon

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/nats-io/nats.go/jetstream"
)

// lambdaHardKillSHA256 pins the hard-kill initializer; tagged test builds set
// it with -ldflags -X, and an unset pin means the test link does not serve.
var lambdaHardKillSHA256 string

// lambdaImageDigest pins the runtime image the same way; it identifies the
// image bytes and does not authenticate where they came from.
var lambdaImageDigest string

// lambdaFirecrackerSHA256 pins the Firecracker v1.17.0 x86_64 release binary.
// A node whose test-link image directory holds a firecracker binary boots
// every attempt under it, and does not serve unless the binary matches.
const lambdaFirecrackerSHA256 = "99ad0f5cd0514a88aad0e9ae8cfdb3cc3b4ab9d190e1194602406c786b5de7a5"

// checkFirecracker refuses a Firecracker binary other than the pinned release.
func checkFirecracker(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("firecracker binary: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("firecracker binary: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != lambdaFirecrackerSHA256 {
		return errors.New("firecracker binary does not match the pinned release")
	}
	return nil
}

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
	imageDir := filepath.Join(microvmDir, "lambda-testlink")
	tl := &testlink.Daemon{
		Node:    d.node,
		Grants:  grants,
		Taps:    taps,
		IPs:     d.mgmtIPAllocator,
		Bridge:  d.mgmtBridgeName(),
		HostIP:  d.mgmtBridgeIP,
		WorkDir: workDir,

		Initializer:       filepath.Join(imageDir, "hardkill.so"),
		InitializerSHA256: lambdaHardKillSHA256,
		HTTP:              httpClient,
	}
	fc := filepath.Join(imageDir, "firecracker")
	if _, err := os.Lstat(fc); err == nil {
		tl.Firecracker = fc
	}
	err = runLambdaTestLink(
		func() { go tl.RecoverLoop(d.ctx, testlink.RecoverFirst, testlink.RecoverMax) },
		func() error {
			if tl.Firecracker != "" {
				if err := checkFirecracker(tl.Firecracker); err != nil {
					return err
				}
			}
			return lambdaTestLinkReady(tl, imageDir, lambdaImageDigest, trustErr)
		},
		func() error {
			gatewayUID, err := testlink.LookupUID(testlink.GatewayUser)
			if err != nil {
				return err
			}
			return tl.ServeControl(d.ctx, testlink.ControlName, gatewayUID)
		},
		func() { releaseLambdaTestLinkImage(tl) },
	)
	if err != nil {
		slog.Error("Lambda test link: not serving new invocations; recovery continues", "err", err)
		return
	}
	slog.Warn("Lambda test link serving: test-only, not for production; the guest does not verify owner grant authority",
		"node", d.node, "image_digest", tl.Image.Digest)
	if d.lambdaMetaService != nil && d.clusterConfig != nil {
		d.lambdaMetaService.SetReadiness(tl.Readiness(lambdaImageDigest, len(d.clusterConfig.Nodes)))
		go d.lambdaMetaService.RunActivation(d.ctx, lambdaActivationInterval)
	}
}

// lambdaActivationInterval bounds how long a function whose activation was
// missed, by a crash or an earlier refusal, waits for the next pass.
const lambdaActivationInterval = 30 * time.Second


// lambdaTestLinkReady runs the serving checks and, only once they pass,
// prepares the pinned image into tl; a refusal leaves tl.Image nil.
func lambdaTestLinkReady(tl *testlink.Daemon, imageDir, imagePin string, trustErr error) error {
	if tl.HostIP == "" {
		return errors.New("management bridge address unavailable")
	}
	if tl.InitializerSHA256 == "" {
		return errors.New("hard-kill initializer digest not pinned")
	}
	if trustErr != nil {
		return trustErr
	}
	if !testlink.ValidDigest(imagePin) {
		return errors.New("runtime image digest not pinned")
	}
	if err := tl.CheckInitializer(); err != nil {
		return err
	}
	im, err := testlink.PrepareImage(imageDir, imagePin)
	if err != nil {
		return err
	}
	tl.Image = im
	return nil
}

// releaseLambdaTestLinkImage closes whatever ready prepared.
func releaseLambdaTestLinkImage(tl *testlink.Daemon) {
	tl.Image.Close()
	tl.Image = nil
}

// runLambdaTestLink starts recovery before any serving check, so a failed
// check refuses new invocations without stopping cleanup of earlier ones.
// release drops whatever ready prepared when ready or serve fails.
func runLambdaTestLink(startRecovery func(), ready, serve func() error, release func()) error {
	startRecovery()
	if err := ready(); err != nil {
		release()
		return err
	}
	if err := serve(); err != nil {
		release()
		return err
	}
	return nil
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
