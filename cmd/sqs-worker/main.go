// sqs-worker runs inside the shared SQS worker microVM.
//
// Usage:
//
//	sqs-worker boot
//
// boot selects and mounts the worker's data disk named by fw_cfg, then loads
// or creates the worker's store key on it and publishes only the public key.
package main

import (
	"fmt"
	"os"

	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerboot"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "boot" {
		fmt.Fprintln(os.Stderr, "usage: sqs-worker boot")
		os.Exit(2)
	}
	logf := func(msg string) { fmt.Fprintf(os.Stderr, "[sqs-worker] %s\n", msg) }
	_, err := workerboot.Boot(workerboot.Config{
		FwCfgDir:      "/sys/firmware/qemu_fw_cfg/by_name/opt/spinifex/sqs-worker",
		SysBlock:      "/sys/block",
		DevDir:        "/dev",
		MountInfo:     "/proc/self/mountinfo",
		DataDir:       "/data",
		RunDir:        "/run/sqs-worker",
		Disk:          workerboot.SystemDisk{E2fsck: "/sbin/e2fsck"},
		IsBlockDevice: workerboot.IsBlockDevice,
		Log:           logf,
	})
	if err != nil {
		logf("FATAL: " + err.Error())
		os.Exit(1)
	}
}
