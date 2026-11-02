package daemon

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The QEMU argument vectors the LB and SQS guests launched with before the
// microVM builder moved to runtime/compute/vm. Any change here is a change
// to how existing system instances boot.
func TestBuildDirectBootConfig_ArgvUnchanged(t *testing.T) {
	mdir := microvmDir
	nics := []string{
		"-device", "virtio-net-pci,netdev=net0,mac=02:aa:bb:cc:dd:01,rx_queue_size=1024",
		"-device", "virtio-net-pci,netdev=net1,mac=02:aa:bb:cc:dd:02,rx_queue_size=1024",
		"-device", "virtio-net-pci,netdev=net2,mac=02:aa:bb:cc:dd:03,rx_queue_size=1024",
	}
	head := func(name string) []string {
		return append([]string{"-name", "guest=" + name + ",debug-threads=on", "-action", "shutdown=pause",
			"-enable-kvm", "-cpu", "host,-pmu", "-display", "none", "-smp", "1", "-m", "128"}, nics...)
	}
	taps := func(mgmt string) []string {
		return []string{
			"-netdev", "tap,id=net0,ifname=tappci000000000,script=no,downscript=no,vhost=on",
			"-netdev", "tap,id=net1,ifname=" + mgmt + ",script=no,downscript=no,vhost=on",
			"-netdev", "tap,id=net2,ifname=tap-unknown-2,script=no,downscript=no,vhost=on",
		}
	}
	const machine = "microvm,pic=off,pit=off,rtc=on,acpi=on,isa-serial=on,pcie=on"
	const tail = " tsc=reliable no_timer_check reboot=t i8042.nopnp i8042.noaux i8042.nokbd"

	cases := []struct {
		name  string
		guest sqsGuest
		want  func(run string) []string
	}{
		{"i-golden-lb", nil, func(run string) []string {
			a := append(head("i-golden-lb"), taps("mggolden-lb")...)
			return append(a,
				"-bios", mdir+"/qboot.rom", "-kernel", mdir+"/vmlinuz", "-initrd", mdir+"/initramfs.cpio.gz",
				"-append", "console=ttyS0 quiet loglevel=7 mitigations=off"+tail,
				"-fw_cfg", "name=opt/spinifex/netcfg,file="+run+"/fwcfg-i-golden-lb-netcfg.tmp",
				"-fw_cfg", "name=opt/spinifex/lb-agent-env,file="+run+"/fwcfg-i-golden-lb-lbenv.tmp",
				"-fw_cfg", "name=opt/spinifex/ca-cert,file="+run+"/fwcfg-i-golden-lb-cacert.tmp",
				"-M", machine)
		}},
		{"i-golden-sqs", &sqsWorkerLaunch{volumeID: "vol-pci", region: "ap-southeast-2", gatewayURL: "https://gw.example:9999", caCert: "ca"}, func(run string) []string {
			a := append(head("i-golden-sqs"), taps("mggolden-sqs")...)
			return append(a,
				"-bios", mdir+"/qboot.rom", "-kernel", mdir+"/sqs-worker/vmlinuz", "-initrd", mdir+"/sqs-worker/initramfs.cpio.gz",
				"-append", "console=ttyS0 quiet loglevel=3"+tail,
				"-fw_cfg", "name=opt/spinifex/netcfg,file="+run+"/fwcfg-i-golden-sqs-netcfg.tmp",
				"-fw_cfg", "name=opt/spinifex/sqs-worker/data-serial,file="+run+"/fwcfg-i-golden-sqs-sqsw-data-serial.tmp",
				"-fw_cfg", "name=opt/spinifex/sqs-worker/region,file="+run+"/fwcfg-i-golden-sqs-sqsw-region.tmp",
				"-fw_cfg", "name=opt/spinifex/sqs-worker/gateway-url,file="+run+"/fwcfg-i-golden-sqs-sqsw-gateway-url.tmp",
				"-fw_cfg", "name=opt/spinifex/ca-cert,file="+run+"/fwcfg-i-golden-sqs-sqsw-cacert.tmp",
				"-M", machine)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := t.TempDir()
			t.Setenv("XDG_RUNTIME_DIR", run)
			d := &Daemon{resourceMgr: &ResourceManager{}}

			cfg, err := d.buildDirectBootConfig(c.name, pciTestInput(), c.guest)
			require.NoError(t, err)
			cmd, err := cfg.Execute()
			require.NoError(t, err)
			assert.Equal(t, c.want(run), cmd.Args[1:])
			for _, e := range cfg.FwCfg {
				assert.NoError(t, os.Remove(e.File))
			}
		})
	}
}
