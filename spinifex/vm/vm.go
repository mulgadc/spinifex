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

	"github.com/aws/aws-sdk-go/service/ec2"
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
	PID          int           `json:"pid"`
	Running      bool          `json:"running"`
	Status       InstanceState `json:"status"`
	InstanceType string        `json:"instance_type"`
	Config       Config        `json:"config"`

	EBSRequests types.EBSRequests `json:"ebs_requests"`

	QMPClient *qmp.QMPClient `json:"-"`

	// User attributes (user initiated stop/delete)
	Attributes types.EC2CommandAttributes `json:"attributes"`

	// EC2 API metadata - stored for AWS API compatibility
	// RunInstancesInput contains the original request parameters (ImageId, KeyName, UserData, etc.)
	RunInstancesInput *ec2.RunInstancesInput `json:"run_instances_input,omitempty"`
	// Reservation contains the reservation metadata (ReservationId, OwnerId, etc.)
	Reservation *ec2.Reservation `json:"reservation,omitempty"`
	// Instance contains the current instance state and metadata
	Instance *ec2.Instance `json:"instance,omitempty"`

	// LastNode records which daemon node last ran this instance.
	// Set when ownership is released on stop for shared KV storage.
	LastNode string `json:"last_node,omitempty"`

	// User data for cloud-init (decoded from base64)
	UserData string `json:"user_data,omitempty"`

	// Metadata server address (e.g., "127.0.0.1:12345") for EC2 metadata service
	MetadataServerAddress string `json:"metadata_server_address,omitempty"`

	// Health tracks crash detection and auto-restart state
	Health InstanceHealthState `json:"health"`

	// AccountID is the AWS account that owns this instance.
	// Empty for pre-Phase4 resources (treated as visible to all accounts).
	AccountID string `json:"account_id,omitempty"`

	// VPC networking: ENI attached to this instance (set by RunInstances when VPC mode is active)
	ENIId  string `json:"eni_id,omitempty"`
	ENIMac string `json:"eni_mac,omitempty"`

	// ExtraENIs lists additional VPC NICs beyond the primary ENIId/ENIMac.
	// Used by multi-AZ system VMs (ALBs with subnets in multiple subnets) —
	// each entry gets its own tap device on br-int and its own QEMU NIC.
	// Empty for customer EC2 instances and single-subnet ALBs.
	ExtraENIs []ExtraENI `json:"extra_enis,omitempty"`

	// Public IP auto-assigned from external IPAM pool (released on termination)
	PublicIP     string `json:"public_ip,omitempty"`
	PublicIPPool string `json:"public_ip_pool,omitempty"`

	// DevMAC is the MAC for the dev/hostfwd NIC (DEV_NETWORKING mode).
	// Set before cloud-init ISO generation so netplan can suppress its default route.
	DevMAC string `json:"dev_mac,omitempty"`

	// Management NIC for system instance control plane (reaches host via br-mgmt).
	MgmtMAC string `json:"mgmt_mac,omitempty"` // MAC address (02:a0:00 prefix)
	MgmtIP  string `json:"mgmt_ip,omitempty"`  // Static IP on management subnet
	MgmtTap string `json:"mgmt_tap,omitempty"` // TAP device name on host

	// Placement group tracking (set during RunInstances when a placement group is specified)
	PlacementGroupName string `json:"placement_group_name,omitempty"`
	PlacementGroupNode string `json:"placement_group_node,omitempty"`

	// ExtraHostfwdPorts lists additional guest ports to forward from the host
	// via the QEMU user-mode dev NIC. Used by ALB VMs to expose HTTP ports.
	// Maps guest port → host port (host port filled in by StartInstance).
	ExtraHostfwd map[int]int `json:"extra_hostfwd,omitempty"`
}

// ResetNodeLocalState zeroes out fields that are specific to the daemon node
// that last ran this instance. Must be called after deserializing a VM from
// shared KV before launching it on a new node.
func (v *VM) ResetNodeLocalState() {
	v.PID = 0
	v.Running = false
	v.MetadataServerAddress = ""
	v.QMPClient = &qmp.QMPClient{}
	v.EBSRequests.Mu = sync.Mutex{}
}

type Instances struct {
	VMS map[string]*VM `json:"vms"`
	Mu  sync.Mutex     `json:"-"`
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
}

func (cfg *Config) Execute() (*exec.Cmd, error) {
	args := []string{}

	if cfg.PIDFile != "" {
		args = append(args, "-pidfile", cfg.PIDFile)
	}

	if cfg.QMPSocket != "" {
		args = append(args, "-qmp", fmt.Sprintf("unix:%s,server,nowait", cfg.QMPSocket))
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

	if cfg.SerialSocket != "" && cfg.ConsoleLogPath != "" {
		chardevOpts := fmt.Sprintf("socket,id=console0,path=%s,server=on,wait=off,logfile=%s",
			cfg.SerialSocket, cfg.ConsoleLogPath)
		args = append(args, "-chardev", chardevOpts, "-serial", "chardev:console0")
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

	if len(cfg.Drives) == 0 {
		return nil, fmt.Errorf("at least one drive is required")
	}

	for _, drive := range cfg.Drives {
		var opts []string

		//args = append(args, "-drive", fmt.Sprintf("file=%s", drive.File)

		if drive.File != "" {
			opts = append(opts, fmt.Sprintf("file=%s", drive.File))
		}

		if drive.Format != "" {
			opts = append(opts, fmt.Sprintf("format=%s", drive.Format))
		}

		if drive.If != "" {
			opts = append(opts, fmt.Sprintf("if=%s", drive.If))
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

	var qemuArchitecture string

	switch cfg.Architecture {
	case "arm64":
		qemuArchitecture = "qemu-system-aarch64"

	case "x86_64":
		qemuArchitecture = "qemu-system-x86_64"

	default:
		return nil, fmt.Errorf("architecture missing")
	}

	// Note, require `-M` machine type for ARM (virt) if set to q35 (incompatible)
	if cfg.Architecture == "arm64" && cfg.MachineType == "q35" {
		args = append(args, "-M", "virt")

		// For ARM, when using virt, preload the firmware file
		// Note: this requires the `qemu-efi-aarch64` package to be installed on the host
		uefiPath := "/usr/share/qemu-efi-aarch64/QEMU_EFI.fd"

		// TODO: Use EFI via NBD for state persistence
		if _, err := os.Stat(uefiPath); err == nil {
			args = append(args, "-bios", uefiPath)
		} else {
			slog.Warn("UEFI firmware file not found for ARM virt machine. Ensure qemu-efi-aarch64 package is installed.", "path", uefiPath)
			return nil, fmt.Errorf("UEFI firmware file not found for ARM virt machine")
		}
	} else if cfg.MachineType != "" {
		args = append(args, "-M", cfg.MachineType)
	}

	slog.Info("Executing QEMU command:", "cmd", qemuArchitecture, "args", args)

	cmd := exec.Command(qemuArchitecture, args...)

	//cmd.Stdout = os.Stdout
	//cmd.Stderr = os.Stderr

	return cmd, nil
}
