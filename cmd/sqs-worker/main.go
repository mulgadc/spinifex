// sqs-worker runs inside the shared SQS worker microVM.
//
// Usage:
//
//	sqs-worker boot
//	sqs-worker ready
//
// boot selects and mounts the worker's data disk named by fw_cfg, asks the
// gateway for its current first-store decision with its instance-role
// credentials, then loads or creates the store key on that disk and
// publishes only the public key. ready then keeps checking the bound store's
// JetStream with that key until it is stopped, keeping a readiness marker only
// while the check passes; it proves the store accepted the worker key, not
// the store server's identity. ready also serves SQS owner calls against
// that store, pulling them from the gateway with the same credentials.
//
// Serving is refused unless the gateway reports a single-node host cluster.
// A disk with no receipt key refuses to serve; recovering it needs a safe
// procedure not yet designed, and nothing deletes or replaces the store.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/mulgadc/spinifex/internal/sqsworkergw"
	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerboot"
	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerserve"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
)

// The cluster CA the daemon supplies; the only root the client trusts.
const caPath = "/sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/ca-cert/raw"

// newClaimer signs with the IMDS instance-role credentials only; there is
// no static-key fallback.
func newClaimer() workerboot.NewClaimer {
	return func(region, gatewayURL string) (workerkey.Claimer, error) {
		return newClient(region, gatewayURL)
	}
}

func newClient(region, gatewayURL string) (*sqsworkergw.Client, error) {
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read cluster CA: %w", err)
	}
	return sqsworkergw.New(gatewayURL, region, ca, gwsign.NewInstanceRole())
}

type describer struct{ c *sqsworkergw.Client }

func (d describer) DescribeStore(ctx context.Context) (workerboot.Store, error) {
	st, err := d.c.DescribeStore(ctx)
	return workerboot.Store(st), err
}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "boot" && os.Args[1] != "ready") {
		fmt.Fprintln(os.Stderr, "usage: sqs-worker boot|ready")
		os.Exit(2)
	}
	logf := func(msg string) { fmt.Fprintf(os.Stderr, "[sqs-worker] %s\n", msg) }
	run := boot
	if os.Args[1] == "ready" {
		run = ready
	}
	if err := run(logf); err != nil {
		logf("FATAL: " + err.Error())
		os.Exit(1)
	}
}

func config(logf func(string)) workerboot.Config {
	return workerboot.Config{
		FwCfgDir:      "/sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/sqs-worker",
		SysBlock:      "/sys/block",
		DevDir:        "/dev",
		MountInfo:     "/proc/self/mountinfo",
		DataDir:       "/data",
		RunDir:        "/run/sqs-worker",
		Disk:          workerboot.SystemDisk{E2fsck: "/sbin/e2fsck"},
		IsBlockDevice: workerboot.IsBlockDevice,
		NewClaimer:    newClaimer(),
		Attempts:      60,
		Wait:          5 * time.Second,
		Log:           logf,
	}
}

func boot(logf func(string)) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err := workerboot.Boot(ctx, config(logf))
	return err
}

// ready runs until init stops it; the marker is removed on the way out.
func ready(logf func(string)) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	// The store is one guest, so its streams and buckets are R1.
	if err := clustersize.Declare(1); err != nil {
		return err
	}
	err := workerboot.Watch(ctx, workerboot.ReadyConfig{
		Config: config(logf),
		NewDescriber: func(region, gatewayURL string) (workerboot.StoreDescriber, error) {
			c, err := newClient(region, gatewayURL)
			if err != nil {
				return nil, err
			}
			return describer{c}, nil
		},
		Checker:     workerboot.NATSChecker{},
		CallTimeout: 30 * time.Second,
		Refresh:     30 * time.Second,
		Refused:     5 * time.Minute,
		Serve: &workerboot.ServeConfig{
			Connector: workerboot.NATSConnector{},
			Run: func(ctx context.Context, region, gatewayURL string, js jetstream.JetStream, receiptKey []byte) error {
				c, err := newClient(region, gatewayURL)
				if err != nil {
					return err
				}
				return workerserve.Run(ctx, c, js, receiptKey, logf)
			},
		},
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
