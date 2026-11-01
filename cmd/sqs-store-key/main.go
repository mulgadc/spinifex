// sqs-store-key runs inside the SQS store microVM after its data disk is
// mounted and before nats-server starts.
//
// Usage:
//
//	sqs-store-key <key-dir> <cert-path>
//
// It loads or creates the store's TLS server key under key-dir on the data
// disk, records only the key's public pin with the gateway using the store's
// instance-role credentials, and writes a self-signed certificate for the key
// to cert-path. A recorded pin with a missing or different key refuses; no key
// is regenerated and nothing on the disk is deleted. The host still holds the
// data disk's key, so this does not protect the store from a privileged host.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/mulgadc/spinifex/internal/sqsworkergw"
	"github.com/mulgadc/spinifex/spinifex/agents/sqs/storekey"
)

const (
	fwcfgDir = "/sys/firmware/qemu_fw_cfg/by_name/opt/spinifex"
	maxValue = 512
)

type gateway struct{ c *sqsworkergw.Client }

func (g gateway) DescribeServerKey(ctx context.Context) (storekey.Record, error) {
	k, err := g.c.DescribeServerKey(ctx)
	return storekey.Record(k), err
}

func (g gateway) RecordServerKey(ctx context.Context, claimID, volumeID, pin string) error {
	return g.c.RecordServerKey(ctx, claimID, volumeID, pin)
}

func main() {
	logf := func(msg string) { fmt.Fprintf(os.Stderr, "[sqs-store-key] %s\n", msg) }
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: sqs-store-key <key-dir> <cert-path>")
		os.Exit(2)
	}
	path, err := run(os.Args[1], os.Args[2], logf)
	if err != nil {
		logf("FATAL: " + err.Error())
		os.Exit(exitCode(err))
	}
	fmt.Println(path)
}

func run(dir, certPath string, logf func(string)) (string, error) {
	serial, err := value("sqs-store/data-serial")
	if err != nil {
		return "", err
	}
	if len(serial) != 20 || !strings.HasPrefix(serial, "vol") {
		return "", errors.New("data-serial is not a volume serial")
	}
	region, err := value("sqs-store/region")
	if err != nil {
		return "", err
	}
	gatewayURL, err := value("sqs-store/gateway-url")
	if err != nil {
		return "", err
	}
	addr, err := value("sqs-store/listen-addr")
	if err != nil {
		return "", err
	}
	ca, err := os.ReadFile(filepath.Join(fwcfgDir, "ca-cert", "raw"))
	if err != nil {
		return "", fmt.Errorf("read cluster CA: %w", err)
	}
	c, err := sqsworkergw.New(gatewayURL, region, ca, gwsign.NewInstanceRole())
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return storekey.Ensure(ctx, storekey.Config{
		Dir:      dir,
		VolumeID: "vol-" + serial[3:],
		Address:  addr,
		CertPath: certPath,
		Gateway:  gateway{c},
		Attempts: 12,
		Wait:     5 * time.Second,
		Log:      logf,
	})
}

// exitCode is 3 for a key that must never be used, which the store treats as
// final, and 1 for anything a later boot step may get past.
func exitCode(err error) int {
	for _, final := range []error{storekey.ErrWrongStore, storekey.ErrKeyMissing, storekey.ErrKeyChanged,
		storekey.ErrUnsafe, storekey.ErrCorrupt} {
		if errors.Is(err, final) {
			return 3
		}
	}
	return 1
}

// value reads one fw_cfg value, refusing a missing, empty or oversized file.
func value(name string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(fwcfgDir, name, "raw"))
	if err != nil {
		return "", fmt.Errorf("fw_cfg %s not supplied", name)
	}
	v := strings.TrimSpace(string(raw))
	if v == "" || len(v) > maxValue || strings.ContainsAny(v, " \t\n") {
		return "", fmt.Errorf("fw_cfg %s must be exactly one value", name)
	}
	return v, nil
}
