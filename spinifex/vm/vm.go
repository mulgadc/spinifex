package vm

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	"github.com/mulgadc/spinifex/spinifex/types"
)

// InstanceHealthState tracks crash detection and auto-restart metadata for a VM.
type InstanceHealthState struct {
	CrashCount      int       `json:"crash_count"`
	LastCrashTime   time.Time `json:"last_crash_time"`
	LastCrashReason string    `json:"last_crash_reason,omitempty"`
	RestartCount    int       `json:"restart_count"`
	FirstCrashTime  time.Time `json:"first_crash_time"`
}

// ExtraENI describes an additional VPC network interface attached to a VM
// beyond the primary ENI. Only system VMs (ALBs) use multiple ENIs today.
type ExtraENI struct {
	ENIID    string `json:"eni_id"`
	ENIMac   string `json:"eni_mac"`
	ENIIP    string `json:"eni_ip"`
	SubnetID string `json:"subnet_id,omitempty"`
}

type VM struct {
	ID           string        `json:"id"`
	Status       InstanceState `json:"status"`
	InstanceType string        `json:"instance_type"`
	Config       Config        `json:"config"`

	EBSRequests types.EBSRequests `json:"ebs_requests"`
	ENIRequests types.ENIRequests `json:"eni_requests"`

	QMPClient *qmp.QMPClient `json:"-"`

	// User attributes (user initiated stop/delete)
	Attributes types.EC2CommandAttributes `json:"attributes"`

	// EC2 API metadata stored for AWS API compatibility.
	RunInstancesInput *ec2.RunInstancesInput `json:"run_instances_input,omitempty"`
	Reservation       *ec2.Reservation       `json:"reservation,omitempty"`
	Instance          *ec2.Instance          `json:"instance,omitempty"`

	// LastNode records which daemon node last ran this instance.
	// Set when ownership is released on stop for shared KV storage.
	LastNode string `json:"last_node,omitempty"`

	// Metadata server address (e.g., "127.0.0.1:12345") for EC2 metadata service
	MetadataServerAddress string `json:"metadata_server_address,omitempty"`

	// Health tracks crash detection and auto-restart state
	Health InstanceHealthState `json:"health"`

	// AccountID is the AWS account that owns this instance. Empty for legacy
	// resources visible only to the root account.
	AccountID string `json:"account_id,omitempty"`

	// VPC networking: ENI attached to this instance (set by RunInstances when VPC mode is active)
	ENIId  string `json:"eni_id,omitempty"`
	ENIMac string `json:"eni_mac,omitempty"`

	// ExtraENIs lists additional VPC NICs beyond the primary ENI.
	// Each gets its own tap on br-int. Empty for single-ENI instances.
	ExtraENIs []ExtraENI `json:"extra_enis,omitempty"`

	// Public IP auto-assigned from external IPAM pool (released on termination)
	PublicIP     string `json:"public_ip,omitempty"`
	PublicIPPool string `json:"public_ip_pool,omitempty"`

	// PublicIPAllocID / PublicIPAssocID are set when the IP was allocated via the
	// EIP service; teardown must go through DisassociateAddress + ReleaseAddress.
	PublicIPAllocID string `json:"public_ip_alloc_id,omitempty"`
	PublicIPAssocID string `json:"public_ip_assoc_id,omitempty"`

	// DevMAC is the MAC for the dev/hostfwd NIC (DEV_NETWORKING mode).
	// Set before cloud-init ISO generation so netplan can suppress its default route.
	DevMAC string `json:"dev_mac,omitempty"`

	// Management NIC for system instance control plane (reaches host via br-mgmt).
	// Tap name derived via MgmtTapName(ID); not persisted so terminate-during-launch
	// can still clean up.
	MgmtMAC string `json:"mgmt_mac,omitempty"` // MAC address (02:a0:00 prefix)
	MgmtIP  string `json:"mgmt_ip,omitempty"`  // Static IP on management subnet

	// Placement group tracking (set during RunInstances when a placement group is specified)
	PlacementGroupName string `json:"placement_group_name,omitempty"`
	PlacementGroupNode string `json:"placement_group_node,omitempty"`

	// ExtraHostfwdPorts lists additional guest ports to forward from the host
	// via the QEMU user-mode dev NIC. Used by ALB VMs to expose HTTP ports.
	// Maps guest port → host port (host port filled in by StartInstance).
	ExtraHostfwd map[int]int `json:"extra_hostfwd,omitempty"`

	// ManagedBy identifies the Spinifex platform component that owns this
	// VM (e.g. "elbv2"). Empty for customer-launched instances. The UI
	// filters out tagged VMs from customer-facing listings.
	ManagedBy string `json:"managed_by,omitempty"`

	// IamInstanceProfileArn is the ARN of the instance profile attached at
	// launch or via AssociateIamInstanceProfile. Empty when no profile is
	// attached. Auto-cleared on TerminateInstances; preserved across stop/start.
	IamInstanceProfileArn string `json:"iam_instance_profile_arn,omitempty"`

	// IamInstanceProfileAssociationId is the association ID used by
	// DescribeIamInstanceProfileAssociations. Regenerated on every Associate/Replace.
	IamInstanceProfileAssociationId string `json:"iam_instance_profile_association_id,omitempty"`

	// DirectBoot signals that Config was pre-built by the launcher (microvm path).
	// When true, startQEMU skips buildBaseVMConfig.
	DirectBoot bool `json:"direct_boot,omitempty"`

	// GPUAttachments describes GPU attachments. Each entry has either a PCI address
	// (whole-GPU VFIO passthrough) or an mdev path (MIG slice), never both.
	GPUAttachments []gpu.GPUAttachment `json:"gpu_attachments,omitempty"`

	// BootMode captures the AMI's boot mode at launch so firmware choice survives
	// restarts. Empty for legacy VMs; treated as "bios" by the launch path.
	BootMode string `json:"boot_mode,omitempty"`

	// Teardown records per-dependent terminate progress (dependent → done|
	// pending|failed). Stamped by terminateCleanup; the GC backstop retries
	// pending/failed and purges the record once all dependents are done.
	Teardown map[string]string `json:"teardown,omitempty"`

	// TerminatedAt is when the instance entered StateTerminated. The GC backstop
	// keeps a completed terminated record describable until this is older than
	// the visibility window, after which it may reclaim it early; the bucket's
	// 1h TTL bounds visibility regardless.
	TerminatedAt time.Time `json:"terminated_at,omitzero"`

	// CapacityReservationId is the On-Demand Capacity Reservation this instance
	// was launched into (targeted launch). Empty for instances on general
	// capacity. Drives slot restore to the reservation on stop/terminate.
	CapacityReservationId string `json:"capacity_reservation_id,omitempty"`
}

// ResetNodeLocalState zeroes node-specific fields after deserializing a VM
// from shared KV before launching it on a new node.
func (v *VM) ResetNodeLocalState() {
	v.MetadataServerAddress = ""
	v.QMPClient = &qmp.QMPClient{}
	v.EBSRequests.Mu = sync.Mutex{}
	v.ENIRequests.Mu = sync.Mutex{}
}

// IsTerminationProtected reports whether the instance was launched with
// DisableApiTermination=true and that value has not been cleared via
// ModifyInstanceAttribute. Nil-safe for legacy instances missing
// RunInstancesInput.
func (v *VM) IsTerminationProtected() bool {
	if v == nil || v.RunInstancesInput == nil {
		return false
	}
	return aws.BoolValue(v.RunInstancesInput.DisableApiTermination)
}

type NetDev struct {
	Value string `json:"value"`
}

type Device struct {
	Value string `json:"value"`
}

type Drive struct {
	File   string `json:"file"`
	Format string `json:"format"`
	If     string `json:"if"`
	Media  string `json:"media"`
	ID     string `json:"id"`
	Cache  string `json:"cache,omitempty"`
	// Unit selects the pflash slot when If=="pflash": 0 is the CODE blob,
	// 1 is the per-VM VARS volume. Ignored for non-pflash drives.
	Unit int `json:"unit,omitempty"`
	// ReadOnly emits readonly=on; used for the pflash CODE blob.
	ReadOnly bool `json:"readonly,omitempty"`
}

type IOThread struct {
	ID string `json:"id"`
}

type Config struct {
	Name           string `json:"name"`
	PIDFile        string `json:"pid_file"`
	QMPSocket      string `json:"qmp_socket"`
	EnableKVM      bool   `json:"enable_kvm"`
	NoGraphic      bool   `json:"no_graphic"`
	MachineType    string `json:"machine_type"`
	ConsoleLogPath string `json:"console_log_path,omitempty"`
	SerialSocket   string `json:"serial_socket,omitempty"`
	CPUType        string `json:"cpu_type"`
	CPUCount       int    `json:"cpu_count"`
	Memory         int    `json:"memory"`

	Drives    []Drive    `json:"drives"`
	IOThreads []IOThread `json:"io_threads,omitempty"`

	Devices []Device `json:"devices"`
	NetDevs []NetDev `json:"net_devs"`

	// InstanceType is a friendly name (e.g., t3.micro, t4g.micro)
	InstanceType string `json:"instance_type"`
	Architecture string `json:"architecture"`

	// UseUEFI requests UEFI firmware. Execute() probes FirmwarePathCandidates and
	// returns an error if no pair is found — no silent SeaBIOS fallback.
	UseUEFI bool `json:"use_uefi,omitempty"`

	// SMBIOSUUID, SMBIOSManufacturer, SMBIOSAssetTag emit EC2-shaped DMI so a stock
	// cloud image's cloud-init activates the Ec2 datasource. cloud-init's identify_aws
	// keys on /sys/class/dmi/id/product_uuid starting with "ec2", so SMBIOSUUID must
	// carry that prefix.
	SMBIOSUUID         string `json:"smbios_uuid,omitempty"`
	SMBIOSManufacturer string `json:"smbios_manufacturer,omitempty"`
	SMBIOSAssetTag     string `json:"smbios_asset_tag,omitempty"`

	KernelImage   string       `json:"kernel_image,omitempty"`   // path to vmlinuz; emits -kernel when set
	Initrd        string       `json:"initrd,omitempty"`         // path to initramfs; emits -initrd when set
	KernelCmdline string       `json:"kernel_cmdline,omitempty"` // emits -append when set
	FwCfg         []FwCfgEntry `json:"fw_cfg,omitempty"`         // each emits -fw_cfg name=<name>,file=<path>
}

// FwCfgEntry describes a single QEMU firmware configuration file entry.
type FwCfgEntry struct {
	Name string `json:"name"`
	File string `json:"file"`
}

func (cfg *Config) Execute() (*exec.Cmd, error) {
	args := []string{}

	if cfg.PIDFile != "" {
		args = append(args, "-pidfile", cfg.PIDFile)
	}

	if cfg.QMPSocket != "" {
		args = append(args, "-qmp", fmt.Sprintf("unix:%s,server,nowait", cfg.QMPSocket))
	}

	// EC2-shaped SMBIOS so a stock cloud image's cloud-init selects the Ec2
	// datasource. -uuid sets the system UUID cloud-init's identify_aws keys on.
	if cfg.SMBIOSUUID != "" {
		args = append(args, "-uuid", cfg.SMBIOSUUID)
	}
	if cfg.SMBIOSManufacturer != "" {
		args = append(args, "-smbios", fmt.Sprintf("type=1,manufacturer=%s,serial=%s", cfg.SMBIOSManufacturer, cfg.SMBIOSUUID))
	}
	if cfg.SMBIOSAssetTag != "" {
		args = append(args, "-smbios", fmt.Sprintf("type=3,asset=%s", cfg.SMBIOSAssetTag))
	}

	// Validate native kvm support
	_, err := os.Stat("/dev/kvm")

	if err != nil {
		slog.Warn("Native KVM support not detected on host. Check permissions and host")
		//slog.Warn("Setting -cpu max, `host` CPU type unavailable")
		// Use qemu defaults
		//args = append(args, "-cpu", "max")
	} else {
		if cfg.EnableKVM {
			args = append(args, "-enable-kvm")
		}

		if cfg.CPUType != "" {
			args = append(args, "-cpu", cfg.CPUType)
		}
	}

	if cfg.NoGraphic {
		args = append(args, "-display", "none")
	}

	if IsMMIO(cfg.MachineType) && cfg.ConsoleLogPath != "" {
		// microvm with isa-serial=on: the machine creates the ISA serial device.
		// Use -chardev file so output is captured without a socket client, then
		// -serial chardev:console0 wires it to the existing device (not a new one).
		// Do NOT use -device isa-serial here — that adds a second device (ttyS1)
		// while the kernel talks to ttyS0.
		args = append(args, "-chardev", fmt.Sprintf("file,id=console0,path=%s", cfg.ConsoleLogPath))
		args = append(args, "-serial", "chardev:console0")
	} else if cfg.SerialSocket != "" && cfg.ConsoleLogPath != "" {
		chardevOpts := fmt.Sprintf("socket,id=console0,path=%s,server=on,wait=off,logfile=%s",
			cfg.SerialSocket, cfg.ConsoleLogPath)
		args = append(args, "-chardev", chardevOpts)
		args = append(args, "-serial", "chardev:console0")
	}

	if cfg.CPUCount > 0 {
		args = append(args, "-smp", strconv.Itoa(cfg.CPUCount))
	} else {
		return nil, fmt.Errorf("cpu count is required")
	}

	if cfg.Memory > 0 {
		args = append(args, "-m", strconv.Itoa(cfg.Memory))
	} else {
		return nil, fmt.Errorf("memory is required")
	}

	for _, iot := range cfg.IOThreads {
		args = append(args, "-object", fmt.Sprintf("iothread,id=%s", iot.ID))
	}

	if len(cfg.Drives) == 0 && cfg.KernelImage == "" {
		return nil, fmt.Errorf("at least one drive or a kernel image is required")
	}

	drives := cfg.Drives
	if cfg.UseUEFI {
		codePath, _, _, fwErr := FirmwarePaths(cfg.Architecture)
		if fwErr != nil {
			return nil, fwErr
		}
		// pflash CODE must come before VARS so QEMU loads firmware into the
		// readonly slot first; buildDrives emits the VARS drive (unit 1) from
		// the per-VM EFI viperblock volume.
		drives = append([]Drive{{If: "pflash", Format: "raw", Unit: 0, ReadOnly: true, File: codePath}}, drives...)
	}

	for _, drive := range drives {
		var opts []string

		if drive.File != "" {
			opts = append(opts, fmt.Sprintf("file=%s", drive.File))
		}

		if drive.Format != "" {
			opts = append(opts, fmt.Sprintf("format=%s", drive.Format))
		}

		if drive.If != "" {
			opts = append(opts, fmt.Sprintf("if=%s", drive.If))
		}

		if drive.If == "pflash" {
			opts = append(opts, fmt.Sprintf("unit=%d", drive.Unit))
		}

		if drive.ReadOnly {
			opts = append(opts, "readonly=on")
		}

		if drive.Media != "" {
			opts = append(opts, fmt.Sprintf("media=%s", drive.Media))
		}

		if drive.ID != "" {
			opts = append(opts, fmt.Sprintf("id=%s", drive.ID))
		}

		if drive.Cache != "" {
			opts = append(opts, fmt.Sprintf("cache=%s", drive.Cache))
		}

		args = append(args, "-drive", strings.Join(opts, ","))
	}

	for _, device := range cfg.Devices {
		args = append(args, "-device", device.Value)
	}

	for _, netdev := range cfg.NetDevs {
		args = append(args, "-netdev", netdev.Value)
	}

	if cfg.KernelImage != "" {
		args = append(args, "-kernel", cfg.KernelImage)
	}

	if cfg.Initrd != "" {
		args = append(args, "-initrd", cfg.Initrd)
	}

	if cfg.KernelCmdline != "" {
		args = append(args, "-append", cfg.KernelCmdline)
	}

	for _, fw := range cfg.FwCfg {
		args = append(args, "-fw_cfg", fmt.Sprintf("name=%s,file=%s", fw.Name, fw.File))
	}

	var qemuArchitecture string

	switch cfg.Architecture {
	case "arm64":
		qemuArchitecture = "qemu-system-aarch64"

	case "x86_64":
		qemuArchitecture = "qemu-system-x86_64"

	default:
		return nil, fmt.Errorf("architecture missing")
	}

	// arm64 q35 is incompatible with the q35 PC machine; override to virt.
	// Firmware loading is unified: UseUEFI emits pflash drives above; no
	// SeaBIOS fallback (aarch64 has none, and a UEFI-only x86_64 guest under
	// SeaBIOS panics on missing ESP).
	if cfg.Architecture == "arm64" && cfg.MachineType == "q35" {
		args = append(args, "-M", "virt")
	} else if cfg.MachineType != "" {
		args = append(args, "-M", cfg.MachineType)
	}

	slog.Info("Executing QEMU command:", "cmd", qemuArchitecture, "args", args)

	cmd := exec.Command(qemuArchitecture, args...)

	//cmd.Stdout = os.Stdout
	//cmd.Stderr = os.Stderr

	return cmd, nil
}
