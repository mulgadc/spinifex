// sqs-worker runs inside the shared SQS worker microVM.
//
// Usage:
//
//	sqs-worker boot
//
// boot selects and mounts the worker's data disk named by fw_cfg, asks the
// gateway for its current first-store decision with its instance-role
// credentials, then loads or creates the store key on that disk and
// publishes only the public key.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/mulgadc/spinifex/internal/sqsworkergw"
	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerboot"
	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
)

// The cluster CA the daemon supplies; the only root the client trusts.
const caPath = "/sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/ca-cert/raw"

// newClaimer signs with the IMDS instance-role credentials only; there is
// no static-key fallback.
func newClaimer() workerboot.NewClaimer {
	return func(region, gatewayURL string) (workerkey.Claimer, error) {
		ca, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read cluster CA: %w", err)
		}
		return sqsworkergw.New(gatewayURL, region, ca, gwsign.NewInstanceRole())
	}
}

func main() {
	if len(os.Args) != 2 || os.Args[1] != "boot" {
		fmt.Fprintln(os.Stderr, "usage: sqs-worker boot")
		os.Exit(2)
	}
	logf := func(msg string) { fmt.Fprintf(os.Stderr, "[sqs-worker] %s\n", msg) }
	if err := boot(logf); err != nil {
		logf("FATAL: " + err.Error())
		os.Exit(1)
	}
}

func boot(logf func(string)) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err := workerboot.Boot(ctx, workerboot.Config{
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
	})
	return err
}
