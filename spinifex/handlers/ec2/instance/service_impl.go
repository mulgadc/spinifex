package handlers_ec2_instance

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	"github.com/kdomanski/iso9660"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/types"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/mulgadc/viperblock/viperblock/backends/s3"
	"github.com/nats-io/nats.go"
)

const cloudInitUserDataTemplate = `#cloud-config
{{if .SSHKey}}
users:
  - name: {{.Username}}
    shell: /bin/sh
    groups:
      - {{.SudoGroup}}
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    ssh_authorized_keys:
      - {{.SSHKey}}
{{end}}
ssh_pwauth: false

hostname: {{.Hostname}}
manage_etc_hosts: true

{{if .CACertPEM}}
ca_certs:
  trusted:
    - |
{{.CACertPEM}}
{{end}}

{{if .UserDataCloudConfig}}
{{.UserDataCloudConfig}}
{{end}}

{{if or .RHELWriteFiles .UserDataScript}}
write_files:
{{- if .RHELWriteFiles}}
{{.RHELWriteFiles}}{{end}}{{if .UserDataScript}}
  - path: /tmp/cloud-init-startup.sh
    permissions: '0755'
    content: |
{{.UserDataScript}}{{end}}
{{end}}
{{if or .RHELRunCmd .UserDataScript}}
runcmd:
{{- if .RHELRunCmd}}
{{.RHELRunCmd}}{{end}}{{if .UserDataScript}}
  - [ "/bin/bash", "/tmp/cloud-init-startup.sh" ]
{{end}}
{{end}}
`

const cloudInitMetaTemplate = `# meta-data
instance-id: {{.InstanceID}}
local-hostname: {{.Hostname}}
`

// cloudInitNetworkConfigWildcard enables DHCP on all NICs via wildcard match.
// Used when there's no dual-NIC setup (non-VPC or VPC without DEV_NETWORKING).
// The "e*" glob matches both traditional names (eth0, eth1 — Alpine/older
// kernels) and predictable names (ens3, enp0s3 — systemd-based distros).
const cloudInitNetworkConfigWildcard = `network:
  version: 2
  ethernets:
    allnics:
      match:
        name: "e*"
      dhcp4: true
      dhcp-identifier: mac
`

// generateNetworkConfig produces the cloud-init network-config for the instance.
//
// Per-interface config is generated when eniMAC is present (VPC NIC). This allows
// adding the mgmt NIC with a static IP and optionally the dev NIC with route
// suppression. Without per-interface config, the wildcard fallback does DHCP on
// all NICs — which won't work for the mgmt NIC (no DHCP server on br-mgmt).
//
// extraENIMACs configures additional VPC NICs for multi-subnet system VMs
// (e.g. multi-AZ ALB VMs). Each extra MAC produces a DHCP ethernet block named
// vpc1, vpc2, ... so each interface pulls its address from the subnet it lives in.
//
// The dev NIC still gets an IP via DHCP (needed for hostfwd port forwarding)
// but dhcp4-overrides prevents it from installing routes or DNS.
func generateNetworkConfig(eniMAC, devMAC, mgmtMAC, mgmtIP string, extraENIMACs []string) string {
	if eniMAC == "" {
		return cloudInitNetworkConfigWildcard
	}
	cfg := fmt.Sprintf(`network:
  version: 2
  ethernets:
    vpc0:
      match:
        macaddress: "%s"
      dhcp4: true
      dhcp-identifier: mac
`, eniMAC)

	for i, mac := range extraENIMACs {
		if mac == "" {
			continue
		}
		cfg += fmt.Sprintf(`    vpc%d:
      match:
        macaddress: "%s"
      dhcp4: true
      dhcp-identifier: mac
`, i+1, mac)
	}

	if devMAC != "" {
		cfg += fmt.Sprintf(`    dev0:
      match:
        macaddress: "%s"
      dhcp4: true
      dhcp-identifier: mac
      dhcp4-overrides:
        use-routes: false
        use-dns: false
`, devMAC)
	}

	if mgmtMAC != "" && mgmtIP != "" {
		cfg += fmt.Sprintf(`    mgmt0:
      match:
        macaddress: "%s"
      addresses:
        - "%s/24"
`, mgmtMAC, mgmtIP)
		// Route for multi-node LB mgmt traffic is delivered to the LB microVM
		// via the fw_cfg netcfg blob, not here.
	}

	return cfg
}

type CloudInitData struct {
	Username            string
	SSHKey              string
	Hostname            string
	UserDataCloudConfig string
	UserDataScript      string
	CACertPEM           string
	// DistroFamily selects per-distro branches in the cloud-init template.
	// "debian" | "rhel" | "alpine". Empty falls through to debian rendering
	// (legacy AMIs registered before the field existed).
	DistroFamily string
	// SudoGroup is the OS group cloud-init adds the user to for passwordless
	// sudo. "sudo" on debian/ubuntu/alpine, "wheel" on RHEL-family.
	SudoGroup string
	// RHELWriteFiles is the pre-indented YAML block of NM keyfile entries
	// (one per NIC) appended under the merged write_files: key. Empty on
	// non-RHEL guests; the network-config ISO file carries the YAML netplan
	// equivalent instead.
	RHELWriteFiles string
	// RHELRunCmd is the pre-indented YAML block of runcmd entries (restorecon
	// + nmcli reload/up) needed to load and bring up the keyfiles. Empty on
	// non-RHEL guests.
	RHELRunCmd string
}

// buildRHELCloudInit produces the cloud-init write_files (NM keyfiles) and
// runcmd (restorecon + nmcli) blocks needed to bring up networking on a
// RHEL-family guest. Returns empty strings when eniMAC is empty (wildcard /
// non-VPC path) so NM's default autoconnect handles the interface.
//
// Mirrors generateNetworkConfig's per-interface structure: vpc0 + optional
// vpc1..N for extra ENIs, dev0 (DHCP with route/DNS suppression), mgmt0
// (static). Owning the keyfile bytes directly avoids relying on cloud-init's
// v2-to-NM renderer round-tripping dhcp-identifier: mac → dhcp-client-id=mac,
// which OVN's MAC-keyed DHCP requires.
//
// File mode 0600 root:root is load-bearing — NM ignores keyfiles otherwise.
// restorecon defers to host SELinux policy (no-op on non-SELinux systems);
// nmcli reload forces NM to rescan system-connections after write_files
// completes in cloud-init's config stage, and nmcli up brings each interface
// online without waiting on autoconnect heuristics.
func buildRHELCloudInit(eniMAC, devMAC, mgmtMAC, mgmtIP string, extraENIMACs []string) (writeFiles, runCmd string) {
	if eniMAC == "" {
		return "", ""
	}

	var wf, rc strings.Builder

	rc.WriteString("  - [ restorecon, -R, /etc/NetworkManager/system-connections/ ]\n")
	rc.WriteString("  - [ nmcli, connection, reload ]\n")

	appendRHELDHCPKeyfile(&wf, "vpc0", eniMAC, false)
	rc.WriteString("  - [ nmcli, connection, up, vpc0 ]\n")

	for i, mac := range extraENIMACs {
		if mac == "" {
			continue
		}
		name := fmt.Sprintf("vpc%d", i+1)
		appendRHELDHCPKeyfile(&wf, name, mac, false)
		fmt.Fprintf(&rc, "  - [ nmcli, connection, up, %s ]\n", name)
	}

	if devMAC != "" {
		appendRHELDHCPKeyfile(&wf, "dev0", devMAC, true)
		rc.WriteString("  - [ nmcli, connection, up, dev0 ]\n")
	}

	if mgmtMAC != "" && mgmtIP != "" {
		appendRHELStaticKeyfile(&wf, "mgmt0", mgmtMAC, mgmtIP)
		rc.WriteString("  - [ nmcli, connection, up, mgmt0 ]\n")
	}

	// Trim trailing newline so the template's {{.RHELWriteFiles}} doesn't
	// produce a blank line before the user-script write_files entry on
	// instances that have both.
	return strings.TrimRight(wf.String(), "\n"), strings.TrimRight(rc.String(), "\n")
}

func appendRHELDHCPKeyfile(b *strings.Builder, name, mac string, suppressDefaults bool) {
	fmt.Fprintf(b, "  - path: /etc/NetworkManager/system-connections/%s.nmconnection\n", name)
	b.WriteString("    owner: root:root\n")
	b.WriteString("    permissions: '0600'\n")
	b.WriteString("    content: |\n")
	b.WriteString("      [connection]\n")
	fmt.Fprintf(b, "      id=%s\n", name)
	b.WriteString("      type=ethernet\n")
	b.WriteString("      [ethernet]\n")
	fmt.Fprintf(b, "      mac-address=%s\n", mac)
	b.WriteString("      [ipv4]\n")
	b.WriteString("      method=auto\n")
	b.WriteString("      dhcp-client-id=mac\n")
	if suppressDefaults {
		// Equivalent to netplan dhcp4-overrides {use-routes: false, use-dns: false}:
		// dev NIC gets an IP for hostfwd but must not install a default route or DNS.
		b.WriteString("      never-default=true\n")
		b.WriteString("      ignore-auto-dns=true\n")
	}
	b.WriteString("      [ipv6]\n")
	b.WriteString("      method=disabled\n")
}

func appendRHELStaticKeyfile(b *strings.Builder, name, mac, ip string) {
	fmt.Fprintf(b, "  - path: /etc/NetworkManager/system-connections/%s.nmconnection\n", name)
	b.WriteString("    owner: root:root\n")
	b.WriteString("    permissions: '0600'\n")
	b.WriteString("    content: |\n")
	b.WriteString("      [connection]\n")
	fmt.Fprintf(b, "      id=%s\n", name)
	b.WriteString("      type=ethernet\n")
	b.WriteString("      [ethernet]\n")
	fmt.Fprintf(b, "      mac-address=%s\n", mac)
	b.WriteString("      [ipv4]\n")
	b.WriteString("      method=manual\n")
	fmt.Fprintf(b, "      addresses=%s/24\n", ip)
	b.WriteString("      [ipv6]\n")
	b.WriteString("      method=disabled\n")
}

type CloudInitMetaData struct {
	InstanceID string
	Hostname   string
}

// VolumeInfo holds volume information returned from GenerateVolumes
// for populating BlockDeviceMappings in the EC2 API response
type VolumeInfo struct {
	VolumeId            string
	DeviceName          string
	AttachTime          time.Time
	DeleteOnTermination bool
}

// volumeParams holds parsed block device mapping parameters for volume creation.
type volumeParams struct {
	size                int
	deviceName          string
	volumeType          string
	iops                int
	imageId             string
	snapshotId          string
	deleteOnTermination bool
}

// parseVolumeParams extracts volume parameters from RunInstancesInput,
// applying defaults and resolving AMI-based image IDs.
func parseVolumeParams(input *ec2.RunInstancesInput) volumeParams {
	p := volumeParams{
		size:                4 * 1024 * 1024 * 1024, // 4GB default
		deviceName:          "/dev/vda",
		deleteOnTermination: true, // matches AWS RunInstances behavior
	}

	if len(input.BlockDeviceMappings) > 0 {
		bdm := input.BlockDeviceMappings[0]
		if bdm.DeviceName != nil {
			p.deviceName = *bdm.DeviceName
		}
		if bdm.Ebs != nil {
			if bdm.Ebs.VolumeSize != nil {
				p.size = int(*bdm.Ebs.VolumeSize) * 1024 * 1024 * 1024
			}
			if bdm.Ebs.VolumeType != nil {
				p.volumeType = *bdm.Ebs.VolumeType
			}
			if bdm.Ebs.Iops != nil {
				p.iops = int(*bdm.Ebs.Iops)
			}
			if bdm.Ebs.DeleteOnTermination != nil {
				p.deleteOnTermination = *bdm.Ebs.DeleteOnTermination
			}
		}
	}

	if strings.HasPrefix(*input.ImageId, "ami-") {
		p.imageId = utils.GenerateResourceID("vol")
		p.snapshotId = *input.ImageId
	} else {
		p.imageId = *input.ImageId
	}

	return p
}

// InstanceServiceImpl handles daemon-side EC2 instance operations
type InstanceServiceImpl struct {
	config        *config.Config
	instanceTypes map[string]*ec2.InstanceTypeInfo
	natsConn      *nats.Conn
	objectStore   objectstore.ObjectStore
	vmMgr         *vm.Manager
	resourceMgr   InstanceTypeAllocator
	stoppedStore  StoppedInstanceStore
	volumeDeleter VolumeDeleter
	eniDeleter    ENIDeleter
	ipReleaser    PublicIPReleaser
	gpuClaimer    GPUClaimer
	amiLoader     AMIMetaLoader
	keyValidator  KeyPairValidator
	eniCreator    ENICreator
	ipAllocator   PublicIPAllocator
}

// NewInstanceServiceImpl creates a new instance service implementation for daemon use
func NewInstanceServiceImpl(
	cfg *config.Config,
	instanceTypes map[string]*ec2.InstanceTypeInfo,
	nc *nats.Conn,
	store objectstore.ObjectStore,
	vmMgr *vm.Manager,
	resourceMgr InstanceTypeAllocator,
	stoppedStore StoppedInstanceStore,
) *InstanceServiceImpl {
	return &InstanceServiceImpl{
		config:        cfg,
		instanceTypes: instanceTypes,
		natsConn:      nc,
		objectStore:   store,
		vmMgr:         vmMgr,
		resourceMgr:   resourceMgr,
		stoppedStore:  stoppedStore,
	}
}

// SetTerminationDeps wires the dependencies required by TerminateStoppedInstance.
// Kept separate from the main constructor so handlers needing only read or
// modify paths can construct a service without dragging the VPC/volume stack in.
func (s *InstanceServiceImpl) SetTerminationDeps(vd VolumeDeleter, ed ENIDeleter, pr PublicIPReleaser) {
	s.volumeDeleter = vd
	s.eniDeleter = ed
	s.ipReleaser = pr
}

// SetGPUClaimer wires the GPU passthrough claim/release dependency used by
// StartStoppedInstance. nil disables GPU passthrough for the service.
func (s *InstanceServiceImpl) SetGPUClaimer(g GPUClaimer) {
	s.gpuClaimer = g
}

// SetRunInstancesDeps wires the AMI/key/VPC/IPAM dependencies required by
// RunInstances. Kept separate from the constructor so handlers needing only
// read paths can build the service without dragging the full stack in.
func (s *InstanceServiceImpl) SetRunInstancesDeps(ami AMIMetaLoader, key KeyPairValidator, eni ENICreator, ipam PublicIPAllocator) {
	s.amiLoader = ami
	s.keyValidator = key
	s.eniCreator = eni
	s.ipAllocator = ipam
}

// RunInstance creates a single EC2 instance (called per-instance by daemon)
// Returns the VM struct and EC2 instance metadata
func (s *InstanceServiceImpl) RunInstance(input *ec2.RunInstancesInput) (*vm.VM, *ec2.Instance, error) {
	// Validate instance type exists
	_, exists := s.instanceTypes[*input.InstanceType]
	if !exists {
		return nil, nil, errors.New(awserrors.ErrorInvalidInstanceType)
	}

	instanceId := utils.GenerateResourceID("i")

	// Create new instance structure
	instance := &vm.VM{
		ID:           instanceId,
		Status:       vm.StateProvisioning,
		InstanceType: *input.InstanceType,
	}

	// Create EC2 instance metadata
	ec2Instance := &ec2.Instance{
		State: &ec2.InstanceState{},
	}
	ec2Instance.SetInstanceId(instance.ID)
	ec2Instance.SetInstanceType(*input.InstanceType)
	if input.ImageId != nil {
		ec2Instance.SetImageId(*input.ImageId)
	}
	if input.KeyName != nil {
		ec2Instance.SetKeyName(*input.KeyName)
	}
	ec2Instance.SetLaunchTime(time.Now())
	ec2Instance.State.SetCode(0)
	ec2Instance.State.SetName("pending")

	// Store EC2 API metadata in VM for DescribeInstances compatibility
	instance.RunInstancesInput = input
	instance.Instance = ec2Instance

	return instance, ec2Instance, nil
}

// PrepareRunInstances validates RunInstances input, allocates per-node capacity,
// creates per-instance VM + ec2.Instance metadata, auto-resolves the subnet,
// creates the primary ENI, and auto-assigns a public IP when the subnet has
// MapPublicIpOnLaunch. It does NOT touch vmMgr, WriteState, or NATS
// subscriptions — those are daemon concerns so the daemon can respond to AWS
// between Prepare and Launch (preserving the original respond-then-launch
// timing). Returns the reservation, the prepared VMs, and the instance-type
// info so the caller can deallocate on failure.
func (s *InstanceServiceImpl) PrepareRunInstances(input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, []*vm.VM, *ec2.InstanceTypeInfo, error) {
	if accountID == "" {
		return nil, nil, nil, errors.New(awserrors.ErrorServerInternal)
	}
	if input == nil || input.InstanceType == nil {
		return nil, nil, nil, errors.New(awserrors.ErrorMissingParameter)
	}

	instanceType, exists := s.instanceTypes[*input.InstanceType]
	if !exists {
		slog.Error("PrepareRunInstances: invalid instance type", "InstanceType", *input.InstanceType)
		return nil, nil, nil, errors.New(awserrors.ErrorInvalidInstanceType)
	}

	if input.ImageId == nil || *input.ImageId == "" {
		slog.Error("PrepareRunInstances: missing ImageId")
		return nil, nil, nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if s.amiLoader == nil {
		slog.Error("PrepareRunInstances: AMI loader not initialized")
		return nil, nil, nil, errors.New(awserrors.ErrorServerInternal)
	}
	amiMeta, err := s.amiLoader.GetAMIConfig(*input.ImageId)
	if err != nil {
		slog.Error("PrepareRunInstances: AMI not found", "imageId", *input.ImageId, "err", err)
		return nil, nil, nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	}
	// Caller must own the AMI or it must carry a non-account-ID owner alias
	// (e.g. "self", "spinifex", "") indicating a system/legacy AMI.
	amiOwner := amiMeta.ImageOwnerAlias
	if amiOwner != "" && amiOwner != accountID && utils.IsAccountID(amiOwner) {
		slog.Warn("PrepareRunInstances: AMI not owned by caller", "imageId", *input.ImageId, "amiOwner", amiOwner, "accountID", accountID)
		return nil, nil, nil, errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	}

	if input.KeyName != nil && *input.KeyName != "" {
		if s.keyValidator == nil {
			slog.Error("PrepareRunInstances: key validator not initialized")
			return nil, nil, nil, errors.New(awserrors.ErrorServerInternal)
		}
		if err := s.keyValidator.ValidateKeyPairExists(accountID, *input.KeyName); err != nil {
			slog.Error("PrepareRunInstances: key pair not found", "keyName", *input.KeyName, "err", err)
			return nil, nil, nil, errors.New(awserrors.ErrorInvalidKeyPairNotFound)
		}
	}

	minCount := int(*input.MinCount)
	maxCount := int(*input.MaxCount)

	allocatableCount := s.resourceMgr.CanAllocate(instanceType, maxCount)
	if allocatableCount < minCount {
		slog.Error("PrepareRunInstances: insufficient capacity", "requested", minCount, "available", allocatableCount, "InstanceType", *input.InstanceType)
		return nil, nil, nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	launchCount := allocatableCount
	slog.Info("PrepareRunInstances: count determined", "min", minCount, "max", maxCount, "launching", launchCount)

	var allocatedCount int
	for i := 0; i < launchCount; i++ {
		if err := s.resourceMgr.Allocate(instanceType); err != nil {
			slog.Error("PrepareRunInstances: allocate failed mid-allocation", "allocated", allocatedCount, "err", err)
			break
		}
		allocatedCount++
	}
	if allocatedCount < minCount {
		for i := 0; i < allocatedCount; i++ {
			s.resourceMgr.Deallocate(instanceType)
		}
		slog.Error("PrepareRunInstances: insufficient capacity after allocation", "allocated", allocatedCount, "minCount", minCount)
		return nil, nil, nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}
	launchCount = allocatedCount

	var instances []*vm.VM
	var allEC2Instances []*ec2.Instance
	var lastRunErr error

	for i := 0; i < launchCount; i++ {
		instance, ec2Instance, err := s.RunInstance(input)
		if err != nil {
			slog.Error("PrepareRunInstances: RunInstance failed", "index", i, "err", err)
			lastRunErr = err
			s.resourceMgr.Deallocate(instanceType)
			continue
		}
		instance.BootMode = amiMeta.BootMode

		// Terraform with associate_public_ip_address sends subnet/SG via
		// NetworkInterfaces[0]; lift to top-level so the rest works uniformly.
		if (input.SubnetId == nil || *input.SubnetId == "") &&
			len(input.NetworkInterfaces) > 0 && input.NetworkInterfaces[0] != nil {
			nic := input.NetworkInterfaces[0]
			if nic.SubnetId != nil && *nic.SubnetId != "" {
				input.SubnetId = nic.SubnetId
			}
			if len(input.SecurityGroupIds) == 0 && len(nic.Groups) > 0 {
				input.SecurityGroupIds = nic.Groups
			}
		}

		if (input.SubnetId == nil || *input.SubnetId == "") && s.eniCreator != nil {
			defaultSubnet, dsErr := s.eniCreator.GetDefaultSubnet(accountID)
			if dsErr == nil && defaultSubnet != nil {
				input.SubnetId = aws.String(defaultSubnet.SubnetID)
				slog.Info("PrepareRunInstances: resolved default subnet", "instanceId", instance.ID, "subnetId", defaultSubnet.SubnetID)
			}
		}

		if input.SubnetId != nil && *input.SubnetId != "" && s.eniCreator != nil {
			eniOut, eniErr := s.eniCreator.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
				SubnetId:    input.SubnetId,
				Description: aws.String("Primary network interface for " + instance.ID),
				Groups:      input.SecurityGroupIds,
			}, accountID)
			if eniErr != nil {
				slog.Error("PrepareRunInstances: auto-create ENI failed", "instanceId", instance.ID, "subnetId", *input.SubnetId, "err", eniErr)
				lastRunErr = eniErr
				s.resourceMgr.Deallocate(instanceType)
				continue
			}

			eni := eniOut.NetworkInterface
			instance.ENIId = *eni.NetworkInterfaceId
			instance.ENIMac = *eni.MacAddress

			if _, attachErr := s.eniCreator.AttachENI(accountID, instance.ENIId, instance.ID, 0); attachErr != nil {
				slog.Error("PrepareRunInstances: failed to attach ENI to instance record — ELBv2 target IP resolution will fail", "eniId", instance.ENIId, "instanceId", instance.ID, "err", attachErr)
			}
			ec2Instance.SetPrivateIpAddress(*eni.PrivateIpAddress)
			ec2Instance.SetSubnetId(*input.SubnetId)
			ec2Instance.SetVpcId(*eni.VpcId)
			ec2Instance.SecurityGroups = eni.Groups
			ec2Instance.NetworkInterfaces = []*ec2.InstanceNetworkInterface{
				{
					NetworkInterfaceId: eni.NetworkInterfaceId,
					PrivateIpAddress:   eni.PrivateIpAddress,
					MacAddress:         eni.MacAddress,
					SubnetId:           input.SubnetId,
					VpcId:              eni.VpcId,
					Status:             aws.String("in-use"),
					Groups:             eni.Groups,
					Attachment: &ec2.InstanceNetworkInterfaceAttachment{
						DeviceIndex: aws.Int64(0),
						Status:      aws.String("attached"),
					},
				},
			}

			slog.Info("PrepareRunInstances: auto-created ENI for VPC instance",
				"instanceId", instance.ID,
				"eniId", instance.ENIId,
				"privateIp", *eni.PrivateIpAddress,
				"mac", instance.ENIMac,
			)

			if s.ipAllocator != nil {
				subnet, subErr := s.eniCreator.GetSubnet(accountID, *input.SubnetId)
				if subErr == nil && subnet != nil && subnet.MapPublicIpOnLaunch {
					region := s.config.Region
					az := s.config.AZ
					publicIP, poolName, allocErr := s.ipAllocator.AllocateIP(region, az, "auto_assign", "", *eni.NetworkInterfaceId, instance.ID)
					if allocErr != nil {
						slog.Warn("PrepareRunInstances: failed to allocate public IP", "instanceId", instance.ID, "err", allocErr)
					} else {
						if updateErr := s.eniCreator.UpdateENIPublicIP(accountID, *eni.NetworkInterfaceId, publicIP, poolName); updateErr != nil {
							slog.Warn("PrepareRunInstances: failed to update ENI with public IP", "eniId", *eni.NetworkInterfaceId, "err", updateErr)
						}
						portName := "port-" + *eni.NetworkInterfaceId
						utils.PublishNATEvent(s.natsConn, "vpc.add-nat", *eni.VpcId, publicIP, *eni.PrivateIpAddress, portName, *eni.MacAddress)
						ec2Instance.PublicIpAddress = aws.String(publicIP)
						instance.PublicIP = publicIP
						instance.PublicIPPool = poolName
						slog.Info("PrepareRunInstances: auto-assigned public IP",
							"instanceId", instance.ID,
							"publicIp", publicIP,
							"privateIp", *eni.PrivateIpAddress,
							"pool", poolName,
						)
					}
				}
			}
		}

		instances = append(instances, instance)
		allEC2Instances = append(allEC2Instances, ec2Instance)
	}

	if len(instances) < minCount {
		for range instances {
			s.resourceMgr.Deallocate(instanceType)
		}
		errCode := awserrors.ErrorServerInternal
		if lastRunErr != nil {
			if _, ok := awserrors.ErrorLookup[lastRunErr.Error()]; ok {
				errCode = lastRunErr.Error()
			}
		}
		slog.Error("PrepareRunInstances: failed to create minimum instances", "created", len(instances), "minCount", minCount, "err", errCode)
		return nil, nil, nil, errors.New(errCode)
	}

	reservation := &ec2.Reservation{}
	reservation.SetReservationId(utils.GenerateResourceID("r"))
	reservation.SetOwnerId(accountID)
	reservation.Instances = allEC2Instances

	for _, instance := range instances {
		instance.Reservation = reservation
		instance.AccountID = accountID
		if input.Placement != nil && input.Placement.GroupName != nil && *input.Placement.GroupName != "" {
			instance.PlacementGroupName = *input.Placement.GroupName
		}
	}

	return reservation, instances, instanceType, nil
}

// LaunchRunInstances takes the VMs created by PrepareRunInstances (already
// inserted into vmMgr by the caller) and runs the heavyweight launch loop:
// volume preparation, GPU claim, vmMgr.Run. Failures mark the VM as failed
// and deallocate resources but do not abort the loop — partial success
// matches AWS behaviour where some instances in a reservation may fail.
func (s *InstanceServiceImpl) LaunchRunInstances(instances []*vm.VM, input *ec2.RunInstancesInput, instanceType *ec2.InstanceTypeInfo) {
	var successCount int
	for _, instance := range instances {
		// Skip if a concurrent request terminated the instance during prepare.
		status := s.vmMgr.Status(instance)
		if status != vm.StatePending && status != vm.StateProvisioning {
			slog.Info("LaunchRunInstances: instance state changed during provisioning, skipping launch",
				"instanceId", instance.ID, "status", string(status))
			continue
		}

		// Pre-compute dev MAC so cloud-init can generate per-interface netplan
		// that suppresses the default route on the dev/hostfwd NIC.
		if s.config.Daemon.DevNetworking && instance.ENIId != "" {
			instance.DevMAC = vm.GenerateDevMAC(instance.ID)
		}

		volumeInfos, err := s.GenerateVolumes(input, instance)
		if err != nil {
			slog.Error("LaunchRunInstances: GenerateVolumes failed", "instanceId", instance.ID, "err", err)
			s.vmMgr.MarkFailed(instance, "volume_preparation_failed")
			continue
		}

		instance.Instance.BlockDeviceMappings = make([]*ec2.InstanceBlockDeviceMapping, 0, len(volumeInfos))
		for _, vi := range volumeInfos {
			mapping := &ec2.InstanceBlockDeviceMapping{}
			mapping.SetDeviceName(vi.DeviceName)
			mapping.Ebs = &ec2.EbsInstanceBlockDevice{}
			mapping.Ebs.SetVolumeId(vi.VolumeId)
			mapping.Ebs.SetAttachTime(vi.AttachTime)
			mapping.Ebs.SetDeleteOnTermination(vi.DeleteOnTermination)
			mapping.Ebs.SetStatus("attached")
			instance.Instance.BlockDeviceMappings = append(instance.Instance.BlockDeviceMappings, mapping)
		}

		if s.gpuClaimer != nil && instancetypes.IsGPUType(instanceType) {
			pciAddr, xvga, gpuErr := s.gpuClaimer.Claim(instance.ID)
			if gpuErr != nil {
				slog.Error("LaunchRunInstances: GPU claim failed", "instanceId", instance.ID, "err", gpuErr)
				s.vmMgr.MarkFailed(instance, "gpu_claim_failed")
				continue
			}
			instance.GPUPCIAddresses = []string{pciAddr}
			instance.GPUXVGAEnabled = xvga
			slog.Info("LaunchRunInstances: GPU claimed for instance", "instanceId", instance.ID, "gpu", pciAddr, "xvga", xvga)
		}

		if err := s.vmMgr.Run(instance); err != nil {
			slog.Error("LaunchRunInstances: vmMgr.Run failed", "instanceId", instance.ID, "err", err)
			if len(instance.GPUPCIAddresses) > 0 && s.gpuClaimer != nil {
				if releaseErr := s.gpuClaimer.Release(instance.ID); releaseErr != nil {
					slog.Error("LaunchRunInstances: GPU release failed after launch failure",
						"instanceId", instance.ID, "err", releaseErr)
				}
			}
			s.vmMgr.MarkFailed(instance, "launch_failed")
			continue
		}

		s.vmMgr.UpdateGuestDeviceNames(instance)

		successCount++
		slog.Info("LaunchRunInstances: launched instance", "instanceId", instance.ID)
	}

	slog.Info("LaunchRunInstances: completed", "requested", len(instances), "launched", successCount)
}

// RunInstances satisfies the InstanceService interface for non-daemon callers
// (tests, mocks). The daemon's NATS handler bypasses this method and calls
// PrepareRunInstances + LaunchRunInstances directly so it can respond to AWS
// between the two phases, preserving the original respond-then-launch timing.
func (s *InstanceServiceImpl) RunInstances(input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
	reservation, instances, instanceType, err := s.PrepareRunInstances(input, accountID)
	if err != nil {
		return nil, err
	}
	for _, instance := range instances {
		s.vmMgr.Insert(instance)
	}
	s.LaunchRunInstances(instances, input, instanceType)
	return reservation, nil
}

// RebootInstance handles an ec2.cmd reboot for an instance already running on
// this node. Returns nil on success; the dispatching daemon handler maps the
// returned error to a NATS error response and responds with `{}` on nil.
func (s *InstanceServiceImpl) RebootInstance(instance *vm.VM, command spxtypes.EC2InstanceCommand) error {
	slog.Info("RebootInstance: rebooting instance", "id", command.ID)

	if err := s.vmMgr.Reboot(instance.ID); err != nil {
		switch {
		case errors.Is(err, vm.ErrInstanceNotFound):
			return errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		case errors.Is(err, vm.ErrInvalidTransition):
			slog.Error("RebootInstance: instance not in running state",
				"instanceId", command.ID, "err", err)
			return errors.New(awserrors.ErrorIncorrectInstanceState)
		default:
			slog.Error("RebootInstance: reboot failed", "instanceId", command.ID, "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}

	slog.Info("RebootInstance: rebooted", "instanceId", command.ID)
	return nil
}

// StartInstance handles an ec2.cmd start for an instance that is stopped on
// this node (local vmMgr state, not the cross-node shared-KV path). Returns
// nil on success; the dispatching daemon handler responds with the running
// status payload.
func (s *InstanceServiceImpl) StartInstance(instance *vm.VM, command spxtypes.EC2InstanceCommand) error {
	slog.Info("StartInstance: starting instance", "id", command.ID)

	status := s.vmMgr.Status(instance)
	if status != vm.StateStopped {
		slog.Error("StartInstance: instance not in stopped state", "instanceId", command.ID, "status", status)
		return errors.New(awserrors.ErrorIncorrectInstanceState)
	}

	instanceType, ok := s.resourceMgr.InstanceTypes()[instance.InstanceType]
	if ok {
		if err := s.resourceMgr.Allocate(instanceType); err != nil {
			slog.Error("StartInstance: failed to allocate resources", "id", command.ID, "err", err)
			return errors.New(awserrors.ErrorInsufficientInstanceCapacity)
		}
	}

	// Clear stop attribute before launch so WriteState inside the manager
	// persists the correct attributes. Without this, a daemon restart after
	// a stop→start cycle would see StopInstance=true and skip reconnecting QEMU.
	s.vmMgr.UpdateState(instance.ID, func(v *vm.VM) { v.Attributes = command.Attributes })

	if err := s.vmMgr.Start(instance.ID); err != nil {
		slog.Error("StartInstance: vmMgr.Start failed", "err", err)
		if ok {
			s.resourceMgr.Deallocate(instanceType)
		}
		return errors.New(awserrors.ErrorServerInternal)
	}

	s.vmMgr.UpdateGuestDeviceNames(instance)

	slog.Info("StartInstance: started", "instanceId", instance.ID)
	return nil
}

// StopOrTerminateInstance handles an ec2.cmd stop or terminate for an
// instance running on this node. Validates the transition synchronously,
// stamps the command attributes onto the VM, then dispatches the actual
// Stop/Terminate in a background goroutine so the daemon can ack immediately.
func (s *InstanceServiceImpl) StopOrTerminateInstance(instance *vm.VM, command spxtypes.EC2InstanceCommand) error {
	isTerminate := command.Attributes.TerminateInstance
	action := "Stopping"
	initialState := vm.StateStopping
	if isTerminate {
		action = "Terminating"
		initialState = vm.StateShuttingDown
	}

	slog.Info("StopOrTerminateInstance: "+action, "id", command.ID)

	// Fold the termination-protection check, idempotency check, transition
	// validation, and attribute stamp into one lock acquisition so a racing
	// ModifyInstanceAttribute can't clear protection between the gates. The
	// async lifecycle goroutine below persists on its own state transitions,
	// so no synchronous persist is needed here.
	var (
		protected, raced, stateMismatch bool
		currentState                    vm.InstanceState
	)
	ok := s.vmMgr.UpdateState(instance.ID, func(v *vm.VM) {
		currentState = v.Status
		if isTerminate && v.IsTerminationProtected() {
			protected = true
			return
		}
		if isTerminate && v.Status == vm.StateShuttingDown {
			raced = true
			return
		}
		if !vm.IsValidTransition(v.Status, initialState) {
			stateMismatch = true
			return
		}
		v.Attributes = command.Attributes
	})
	if !ok {
		slog.Warn("StopOrTerminateInstance: instance no longer in running map",
			"instanceId", instance.ID)
		return errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	if protected {
		slog.Warn("StopOrTerminateInstance: instance has termination protection",
			"instanceId", instance.ID)
		return errors.New(awserrors.ErrorOperationNotPermitted)
	}
	if raced {
		// Idempotent: a concurrent terminate goroutine is already cleaning up.
		slog.Info("StopOrTerminateInstance: instance already shutting down, terminate is idempotent", "instanceId", instance.ID)
		return nil
	}
	if stateMismatch {
		// Surface IncorrectInstanceState synchronously so the AWS SDK sees
		// 400 instead of a stale 200. The async path re-validates.
		slog.Warn("StopOrTerminateInstance: instance in incorrect state for "+strings.ToLower(action),
			"instanceId", instance.ID, "currentState", string(currentState))
		return errors.New(awserrors.ErrorIncorrectInstanceState)
	}

	go func(id string) {
		var err error
		if isTerminate {
			err = s.vmMgr.Terminate(id)
		} else {
			err = s.vmMgr.Stop(id)
		}
		if err != nil {
			if errors.Is(err, vm.ErrInvalidTransition) {
				slog.Warn("StopOrTerminateInstance: lifecycle transition raced; ack already sent",
					"id", id, "action", strings.ToLower(action), "err", err)
				return
			}
			slog.Error("StopOrTerminateInstance: failed to "+strings.ToLower(action), "err", err, "id", id)
		}
	}(instance.ID)

	return nil
}

func (s *InstanceServiceImpl) GenerateVolumes(input *ec2.RunInstancesInput, instance *vm.VM) ([]VolumeInfo, error) {
	p := parseVolumeParams(input)

	// Capture attach time for the root volume
	attachTime := time.Now()

	volumeConfig := viperblock.VolumeConfig{
		VolumeMetadata: viperblock.VolumeMetadata{
			VolumeID:            p.imageId,
			SizeGiB:             utils.SafeIntToUint64(p.size / 1024 / 1024 / 1024),
			CreatedAt:           attachTime,
			DeviceName:          p.deviceName,
			VolumeType:          p.volumeType,
			IOPS:                p.iops,
			SnapshotID:          p.snapshotId,
			DeleteOnTermination: p.deleteOnTermination,
			TenantID:            instance.AccountID,
		},
	}

	size := p.size
	imageId := p.imageId
	deviceName := p.deviceName
	deleteOnTermination := p.deleteOnTermination

	// Step 1: Create or validate root volume
	err := s.prepareRootVolume(input, imageId, size, volumeConfig, instance, deleteOnTermination)
	if err != nil {
		return nil, err
	}

	// Step 2: Create EFI variable store (only when the AMI is UEFI; BIOS
	// guests must not allocate an orphan VARS volume).
	if instance.BootMode == "uefi" || instance.BootMode == "uefi-preferred" {
		arch := instanceArchitecture(s.instanceTypes[*input.InstanceType])
		err = s.prepareEFIVolume(imageId, volumeConfig, instance, arch)
		if err != nil {
			return nil, err
		}
	}

	// Step 3: Create cloud-init volume if needed
	if input.KeyName != nil && *input.KeyName != "" || (input.UserData != nil && *input.UserData != "") {
		err = s.prepareCloudInitVolume(input, imageId, volumeConfig, instance)
		if err != nil {
			return nil, err
		}
	}

	// Return volume info for the root volume only (EFI and cloud-init are internal)
	volumeInfos := []VolumeInfo{
		{
			VolumeId:            imageId,
			DeviceName:          deviceName,
			AttachTime:          attachTime,
			DeleteOnTermination: deleteOnTermination,
		},
	}

	return volumeInfos, nil
}

// newViperblock creates a viperblock instance with the service's S3/Predastore credentials.
func (s *InstanceServiceImpl) newViperblock(volumeName string, size int, volumeConfig viperblock.VolumeConfig) (*viperblock.VB, error) {
	cfg := s3.S3Config{
		VolumeName: volumeName,
		VolumeSize: utils.SafeIntToUint64(size),
		Bucket:     s.config.Predastore.Bucket,
		Region:     s.config.Predastore.Region,
		AccessKey:  s.config.Predastore.AccessKey,
		SecretKey:  s.config.Predastore.SecretKey,
		Host:       s.config.Predastore.Host,
	}

	vbconfig := viperblock.VB{
		VolumeName:   volumeName,
		VolumeSize:   utils.SafeIntToUint64(size),
		BaseDir:      s.config.WalDir,
		Cache:        viperblock.Cache{Config: viperblock.CacheConfig{Size: 0}},
		VolumeConfig: volumeConfig,
	}

	vb, err := viperblock.New(&vbconfig, "s3", cfg)
	restoreSlogDefault()
	return vb, err
}

// restoreSlogDefault re-installs the daemon's Info-level slog handler after
// viperblock.New mutates the global slog default via its SetDebug method
// (see viperblock.go SetDebug — it calls slog.SetDefault with LevelError,
// silencing every Info/Warn in the entire process). Tracked for proper fix
// in viperblock as mulga-siv-70.
func restoreSlogDefault() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))
}

// prepareRootVolume handles creation/cloning of the root volume
func (s *InstanceServiceImpl) prepareRootVolume(input *ec2.RunInstancesInput, imageId string, size int, volumeConfig viperblock.VolumeConfig, instance *vm.VM, deleteOnTermination bool) error {
	vb, err := s.newViperblock(imageId, size, volumeConfig)
	if err != nil {
		slog.Error("Failed to connect to Viperblock store", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Initialize the backend
	err = vb.Backend.Init()
	if err != nil {
		slog.Error("Failed to initialize backend", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Load the state from the remote backend
	_, err = vb.LoadStateRequest("")

	// If volume doesn't exist, clone from AMI
	if err != nil {
		slog.Info("Volume does not yet exist, creating from AMI ...")

		err = s.cloneAMIToVolume(input, size, volumeConfig, vb)
		if err != nil {
			return err
		}
	}

	// Append root volume to instance
	instance.EBSRequests.Mu.Lock()
	instance.EBSRequests.Requests = append(instance.EBSRequests.Requests, spxtypes.EBSRequest{
		Name:                imageId,
		Boot:                true,
		DeleteOnTermination: deleteOnTermination,
	})
	instance.EBSRequests.Mu.Unlock()

	return nil
}

// cloneAMIToVolume creates a new volume from an AMI using snapshot-based
// zero-copy cloning. The destination volume points at the AMI's frozen block
// map and reads on-demand from the AMI's chunks (copy-on-write).
func (s *InstanceServiceImpl) cloneAMIToVolume(input *ec2.RunInstancesInput, size int, volumeConfig viperblock.VolumeConfig, destVb *viperblock.VB) error {
	// Load AMI state to get the snapshot ID
	amiVb, err := s.newViperblock(*input.ImageId, size, volumeConfig)
	if err != nil {
		slog.Error("Failed to connect to Viperblock store for AMI", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	err = amiVb.Backend.Init()
	if err != nil {
		slog.Error("Could not connect to AMI Viperblock store", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	amiState, err := amiVb.LoadStateRequest("")
	if err != nil {
		slog.Error("Could not load state for AMI", "imageId", *input.ImageId, "err", err)
		return errors.New(awserrors.ErrorInvalidAMIIDNotFound)
	}

	snapshotID := amiState.VolumeConfig.AMIMetadata.SnapshotID
	if snapshotID == "" {
		slog.Error("AMI has no snapshot ID, cannot perform zero-copy clone", "imageId", *input.ImageId)
		return errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("Cloning AMI via snapshot", "imageId", *input.ImageId, "snapshotID", snapshotID)

	// Set up destination volume from the snapshot (zero-copy)
	err = destVb.OpenFromSnapshot(snapshotID)
	if err != nil {
		slog.Error("Failed to open from snapshot", "snapshotID", snapshotID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Persist the snapshot relationship to the backend
	err = destVb.SaveState()
	if err != nil {
		slog.Error("Failed to save state", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	err = destVb.SaveBlockState()
	if err != nil {
		slog.Error("Failed to save block state", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	err = destVb.RemoveLocalFiles()
	if err != nil {
		slog.Warn("Failed to remove local files", "err", err)
	}

	return nil
}

// prepareEFIVolume creates the per-VM EFI variable store. The volume is
// sized to match the firmware's VARS template (QEMU pflash refuses any other
// size) and seeded with the template bytes so EFI variables — BootOrder,
// BootNext, secure-boot state — survive across reboots. arch is the AMI
// architecture string ("x86_64" | "arm64").
func (s *InstanceServiceImpl) prepareEFIVolume(imageId string, volumeConfig viperblock.VolumeConfig, instance *vm.VM, arch string) error {
	codePath, varsTemplate, varsSize, err := vm.FirmwarePaths(arch)
	if err != nil {
		slog.Error("UEFI firmware not installed on this host", "arch", arch, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	template, err := os.ReadFile(varsTemplate)
	if err != nil {
		slog.Error("Failed to read VARS template", "path", varsTemplate, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if int64(len(template)) != varsSize {
		slog.Error("VARS template size mismatch between stat and read", "path", varsTemplate, "statSize", varsSize, "readSize", len(template))
		return errors.New(awserrors.ErrorServerInternal)
	}
	slog.Info("Preparing EFI variable store", "arch", arch, "code", codePath, "varsTemplate", varsTemplate, "size", varsSize)

	efiVolumeName := fmt.Sprintf("%s-efi", imageId)
	efiVolumeConfig := volumeConfig
	efiVolumeConfig.VolumeMetadata.VolumeID = efiVolumeName
	// Zero SizeGiB so viperblock's LoadState doesn't reconcile the EFI
	// volume's persisted byte-exact size up to the parent root volume's
	// GiB-rounded size — QEMU pflash rejects any VARS volume larger than
	// the firmware's expected variable region.
	efiVolumeConfig.VolumeMetadata.SizeGiB = 0

	efiVb, err := s.newViperblock(efiVolumeName, int(varsSize), efiVolumeConfig)
	if err != nil {
		slog.Error("Could not create EFI viperblock", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	slog.Debug("Initializing EFI Viperblock store backend")
	if err := efiVb.Backend.Init(); err != nil {
		slog.Error("Failed to initialize EFI Viperblock store backend", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Distinguish "not found" (seed from template) from any other backend
	// failure (transient S3 5xx, JSON unmarshal, etc.). Treating a transient
	// failure as "not found" would silently re-seed a volume on every retry,
	// clobbering guest-set BootOrder. Backends signal missing-object by
	// wrapping os.ErrNotExist (see viperblock.classifyStateLoad).
	_, loadErr := efiVb.LoadStateRequest("")
	if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
		slog.Error("Failed to load EFI volume state from backend", "name", efiVolumeName, "err", loadErr)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if loadErr != nil {
		slog.Info("EFI volume does not yet exist, seeding from firmware VARS template", "name", efiVolumeName)

		var walErr error
		if efiVb.UseShardedWAL {
			walErr = efiVb.OpenShardedWAL()
		} else {
			walErr = efiVb.OpenWAL(&efiVb.WAL, fmt.Sprintf("%s/%s", efiVb.WAL.BaseDir, types.GetFilePath(types.FileTypeWALChunk, efiVb.WAL.WallNum.Load(), efiVb.GetVolume())))
		}
		if walErr != nil {
			slog.Error("Failed to load WAL", "err", walErr)
			return errors.New(awserrors.ErrorServerInternal)
		}

		if err := efiVb.OpenWAL(&efiVb.BlockToObjectWAL, fmt.Sprintf("%s/%s", efiVb.WAL.BaseDir, types.GetFilePath(types.FileTypeWALBlock, efiVb.BlockToObjectWAL.WallNum.Load(), efiVb.GetVolume()))); err != nil {
			slog.Error("Failed to load block WAL", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}

		if err := efiVb.WriteAt(0, template); err != nil {
			slog.Error("Failed to seed EFI volume with VARS template", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
		if err := efiVb.Flush(); err != nil {
			slog.Error("Failed to flush EFI volume", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}

	// Close is the durability boundary after WriteAt+Flush; if it fails the
	// VARS volume may be partially written and QEMU pflash will refuse to
	// start. Fail the launch loudly rather than enqueueing a corrupt volume.
	if err := efiVb.Close(); err != nil {
		slog.Error("Failed to close EFI Viperblock store", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if err := efiVb.RemoveLocalFiles(); err != nil {
		slog.Error("Failed to remove local files", "err", err)
	}

	instance.EBSRequests.Mu.Lock()
	instance.EBSRequests.Requests = append(instance.EBSRequests.Requests, spxtypes.EBSRequest{
		Name: efiVb.VolumeName,
		Boot: false,
		EFI:  true,
	})
	instance.EBSRequests.Mu.Unlock()

	return nil
}

// instanceArchitecture pulls the AWS architecture string off an
// ec2.InstanceTypeInfo. Returns "" when the spec is nil or malformed; the
// caller's firmware probe surfaces that as a clear error rather than a
// silent BIOS fallback.
func instanceArchitecture(it *ec2.InstanceTypeInfo) string {
	if it == nil || it.ProcessorInfo == nil || len(it.ProcessorInfo.SupportedArchitectures) == 0 || it.ProcessorInfo.SupportedArchitectures[0] == nil {
		return ""
	}
	return *it.ProcessorInfo.SupportedArchitectures[0]
}

// prepareCloudInitVolume creates cloud-init ISO with SSH keys and user data.
// rootVolumeId is the per-instance root volume ID (not the AMI ID), ensuring
// each instance gets its own cloud-init volume with fresh SSH keys and metadata.
func (s *InstanceServiceImpl) prepareCloudInitVolume(input *ec2.RunInstancesInput, rootVolumeId string, volumeConfig viperblock.VolumeConfig, instance *vm.VM) error {
	slog.Info("Creating cloud-init volume")

	cloudInitVolumeName := fmt.Sprintf("%s-cloudinit", rootVolumeId)
	cloudInitSize := 1 * 1024 * 1024 // 1MB

	// Update VolumeID to match the cloud-init volume name
	cloudInitVolumeConfig := volumeConfig
	cloudInitVolumeConfig.VolumeMetadata.VolumeID = cloudInitVolumeName

	cloudInitVb, err := s.newViperblock(cloudInitVolumeName, cloudInitSize, cloudInitVolumeConfig)
	if err != nil {
		slog.Error("Could not create cloudinit viperblock", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Initialize the backend
	slog.Debug("Initializing cloud-init Viperblock store backend")
	err = cloudInitVb.Backend.Init()
	if err != nil {
		slog.Error("Could not init backend", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Always create a fresh cloud-init ISO — never reuse a cached volume.
	// Each instance needs its own SSH key, hostname, and user data.

	// Open the chunk WAL (sharded or legacy)
	if cloudInitVb.UseShardedWAL {
		err = cloudInitVb.OpenShardedWAL()
	} else {
		err = cloudInitVb.OpenWAL(&cloudInitVb.WAL, fmt.Sprintf("%s/%s", cloudInitVb.WAL.BaseDir, types.GetFilePath(types.FileTypeWALChunk, cloudInitVb.WAL.WallNum.Load(), cloudInitVb.GetVolume())))
	}
	if err != nil {
		slog.Error("Failed to load WAL", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	err = cloudInitVb.OpenWAL(&cloudInitVb.BlockToObjectWAL, fmt.Sprintf("%s/%s", cloudInitVb.WAL.BaseDir, types.GetFilePath(types.FileTypeWALBlock, cloudInitVb.BlockToObjectWAL.WallNum.Load(), cloudInitVb.GetVolume())))
	if err != nil {
		slog.Error("Failed to load block WAL", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Create the cloud-init ISO
	err = s.createCloudInitISO(input, instance, cloudInitVb)
	if err != nil {
		return err
	}

	err = cloudInitVb.Close()
	if err != nil {
		slog.Error("Failed to close cloud-init Viperblock store", "err", err)
	}

	err = cloudInitVb.RemoveLocalFiles()
	if err != nil {
		slog.Error("Failed to remove local files", "err", err)
	}

	instance.EBSRequests.Mu.Lock()
	instance.EBSRequests.Requests = append(instance.EBSRequests.Requests, spxtypes.EBSRequest{
		Name:      cloudInitVolumeName,
		Boot:      false,
		CloudInit: true,
	})
	instance.EBSRequests.Mu.Unlock()

	return nil
}

// createCloudInitISO generates the cloud-init ISO image
func (s *InstanceServiceImpl) createCloudInitISO(input *ec2.RunInstancesInput, instance *vm.VM, cloudInitVb *viperblock.VB) error {
	// Create ISO writer
	writer, err := iso9660.NewWriter()
	if err != nil {
		slog.Error("failed to create writer", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	defer writer.Cleanup()

	// Generate instance metadata
	hostname := generateHostname(instance.ID)

	// Retrieve SSH pubkey from S3 — required for instance access.
	// Password authentication is not supported; instances without a key
	// pair have no remote access method.
	keyName := ""
	if input.KeyName != nil {
		keyName = *input.KeyName
	}

	var sshKey []byte
	if keyName != "" {
		keyPath := fmt.Sprintf("keys/%s/%s", instance.AccountID, keyName)
		result, err := s.objectStore.GetObject(&awss3.GetObjectInput{
			Bucket: aws.String(s.config.Predastore.Bucket),
			Key:    aws.String(keyPath),
		})
		if err != nil {
			if objectstore.IsNoSuchKeyError(err) {
				slog.Error("key pair not found", "keyName", keyName, "err", err)
				return errors.New(awserrors.ErrorInvalidKeyPairNotFound)
			}
			slog.Error("failed to read SSH key", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}

		sshKey, err = io.ReadAll(result.Body)
		if err != nil {
			slog.Error("failed to read SSH key body", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}

	// Read CA certificate for injection into guest cloud-init.
	// Derive the config directory: BaseDir (e.g. ~/spinifex/spinifex/) sits one
	// level below the data root; the CA cert is at <data-root>/config/ca.pem.
	var caCertPEM string
	dataRoot := filepath.Dir(strings.TrimSuffix(s.config.BaseDir, "/"))
	caCertPath := filepath.Join(dataRoot, "config", "ca.pem")
	if caBytes, err := os.ReadFile(caCertPath); err == nil {
		// Indent each line by 6 spaces for YAML block scalar in ca_certs.trusted.
		var indented strings.Builder
		for line := range strings.SplitSeq(string(caBytes), "\n") {
			if line != "" {
				indented.WriteString("      ")
				indented.WriteString(line)
				indented.WriteByte('\n')
			}
		}
		caCertPEM = indented.String()
	} else if os.IsNotExist(err) {
		slog.Warn("CA cert not found, guest VMs will not trust Spinifex services", "path", caCertPath)
	} else {
		slog.Error("failed to read CA cert for guest cloud-init injection", "path", caCertPath, "error", err)
	}

	// Re-read AMI metadata for DistroFamily rather than persisting it on
	// vm.VM — cloud-init renders once at first launch and the ISO is sealed,
	// so no consumer needs DistroFamily after this point. Mirrors how
	// PrepareRunInstances reads amiMeta at service_impl.go:369.
	var distroFamily string
	if s.amiLoader != nil && input.ImageId != nil && *input.ImageId != "" {
		if amiMeta, err := s.amiLoader.GetAMIConfig(*input.ImageId); err == nil {
			distroFamily = amiMeta.DistroFamily
		}
	}

	extraMACs := make([]string, 0, len(instance.ExtraENIs))
	for _, extra := range instance.ExtraENIs {
		extraMACs = append(extraMACs, extra.ENIMac)
	}

	sudoGroup := "sudo"
	var rhelWriteFiles, rhelRunCmd string
	if distroFamily == "rhel" {
		sudoGroup = "wheel"
		rhelWriteFiles, rhelRunCmd = buildRHELCloudInit(instance.ENIMac, instance.DevMAC, instance.MgmtMAC, instance.MgmtIP, extraMACs)
	}

	userData := CloudInitData{
		Username:       "ec2-user",
		SSHKey:         string(sshKey),
		Hostname:       hostname,
		CACertPEM:      caCertPEM,
		DistroFamily:   distroFamily,
		SudoGroup:      sudoGroup,
		RHELWriteFiles: rhelWriteFiles,
		RHELRunCmd:     rhelRunCmd,
	}

	// Decode and classify user-data from RunInstances (base64-encoded).
	if input.UserData != nil && *input.UserData != "" {
		decoded, decErr := base64.StdEncoding.DecodeString(*input.UserData)
		if decErr != nil {
			slog.Warn("Failed to decode user-data, ignoring", "err", decErr)
		} else {
			raw := string(decoded)
			if after, ok := strings.CutPrefix(raw, "#cloud-config"); ok {
				// Strip the #cloud-config header — the template already has it
				stripped := after
				userData.UserDataCloudConfig = strings.TrimSpace(stripped)
			} else {
				// Script — indent each line by 4 spaces for YAML write_files block
				var indented strings.Builder
				for line := range strings.SplitSeq(raw, "\n") {
					indented.WriteString("      " + line + "\n")
				}
				userData.UserDataScript = indented.String()
			}
		}
	}

	var buf bytes.Buffer
	t := template.Must(template.New("cloud-init").Parse(cloudInitUserDataTemplate))

	if err := t.Execute(&buf, userData); err != nil {
		slog.Error("failed to render template", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Add user-data
	err = writer.AddFile(&buf, "user-data")
	if err != nil {
		slog.Error("failed to add file", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Add meta-data
	metaData := CloudInitMetaData{
		InstanceID: instance.ID,
		Hostname:   hostname,
	}

	t = template.Must(template.New("meta-data").Parse(cloudInitMetaTemplate))
	buf.Reset()

	if err := t.Execute(&buf, metaData); err != nil {
		slog.Error("failed to render template", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	err = writer.AddFile(&buf, "meta-data")
	if err != nil {
		slog.Error("failed to add file", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Add network-config: per-interface when VPC+dev (suppresses dev default route),
	// wildcard DHCP otherwise. Extra ENI MACs produce additional DHCP NICs for
	// multi-subnet system VMs (multi-AZ ALBs).
	//
	// RHEL-family guests get their NM keyfiles via user-data write_files
	// instead, so skip network-config here to keep a single writer for
	// /etc/NetworkManager/system-connections/.
	if distroFamily != "rhel" {
		networkConfig := generateNetworkConfig(instance.ENIMac, instance.DevMAC, instance.MgmtMAC, instance.MgmtIP, extraMACs)
		err = writer.AddFile(strings.NewReader(networkConfig), "network-config")
		if err != nil {
			slog.Error("failed to add network-config file", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}
	}

	// Store temp file
	tempFile, err := os.CreateTemp("", "cloud-init-*.iso")
	if err != nil {
		slog.Error("Could not create cloud-init temp file", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("Created temp ISO file", "file", tempFile.Name())

	outputFile, err := os.OpenFile(tempFile.Name(), os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0600)
	if err != nil {
		slog.Error("failed to create file", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Requires cidata volume label for cloud-init to recognize
	err = writer.WriteTo(outputFile, "cidata")
	if err != nil {
		slog.Error("failed to write ISO image", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	err = writer.Cleanup()
	if err != nil {
		slog.Error("failed to cleanup writer", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	err = outputFile.Close()
	if err != nil {
		slog.Error("failed to close output file", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	isoData, err := os.ReadFile(tempFile.Name())
	if err != nil {
		slog.Error("failed to read ISO image:", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	err = cloudInitVb.WriteAt(0, isoData)
	if err != nil {
		slog.Error("failed to write ISO image to viperblock volume", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Flush
	if err := cloudInitVb.Flush(); err != nil {
		slog.Error("Failed to flush cloud-init volume", "err", err)
	}
	if err := cloudInitVb.WriteWALToChunk(true); err != nil {
		slog.Error("Failed to write WAL to chunk", "err", err)
	}

	// Remove the temp ISO file
	err = os.Remove(tempFile.Name())
	if err != nil {
		slog.Error("Failed to remove temp file", "err", err)
	}

	return nil
}

// generateHostname creates a hostname based on instance ID
func generateHostname(instanceID string) string {
	if len(instanceID) > 2 {
		uniquePart := instanceID[2:10] // Take first 8 chars after "i-"
		return fmt.Sprintf("spinifex-vm-%s", uniquePart)
	}
	return "spinifex-vm-unknown"
}

// DescribeInstancesValidFilters lists the filter names accepted by DescribeInstances
// (and the stopped/terminated variants, which share the same filter shape).
var DescribeInstancesValidFilters = map[string]bool{
	"instance-state-name": true,
	"instance-id":         true,
	"instance-type":       true,
	"vpc-id":              true,
	"subnet-id":           true,
	"tag-key":             true,
	"tag-value":           true,
}

// DescribeInstanceStatusValidFilters lists the filter names accepted by
// DescribeInstanceStatus. event.*, instance-status.* and system-status.*
// are rejected: Mulga's static-payload health model has no events and a
// single value per status field, so those filters can't usefully narrow.
var DescribeInstanceStatusValidFilters = map[string]bool{
	"availability-zone":   true,
	"instance-state-code": true,
	"instance-state-name": true,
}

// IsInstanceVisible reports whether the caller can see this instance.
// Pre-Phase4 instances (empty AccountID) are only visible to root (GlobalAccountID).
func IsInstanceVisible(callerAccountID, ownerAccountID string) bool {
	if ownerAccountID == "" {
		return callerAccountID == utils.GlobalAccountID
	}
	return callerAccountID == ownerAccountID
}

// instanceMatchesFilters checks whether a VM + its built ec2.Instance copy satisfy all parsed filters.
func instanceMatchesFilters(inst *vm.VM, ic *ec2.Instance, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}

		var field string
		switch name {
		case "instance-state-name":
			if ic.State != nil && ic.State.Name != nil {
				field = *ic.State.Name
			}
		case "instance-id":
			field = inst.ID
		case "instance-type":
			field = inst.InstanceType
		case "vpc-id":
			if ic.VpcId != nil {
				field = *ic.VpcId
			}
		case "subnet-id":
			if ic.SubnetId != nil {
				field = *ic.SubnetId
			}
		case "tag-key":
			if !matchTagKey(ic.Tags, values) {
				return false
			}
			continue
		case "tag-value":
			if !matchTagValue(ic.Tags, values) {
				return false
			}
			continue
		default:
			return false
		}

		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	tags := filterutil.EC2TagsToMap(ic.Tags)
	return filterutil.MatchesTags(filters, tags)
}

func matchTagKey(tags []*ec2.Tag, values []string) bool {
	for _, t := range tags {
		if t.Key != nil && filterutil.MatchesAny(values, *t.Key) {
			return true
		}
	}
	return false
}

func matchTagValue(tags []*ec2.Tag, values []string) bool {
	for _, t := range tags {
		if t.Value != nil && filterutil.MatchesAny(values, *t.Value) {
			return true
		}
	}
	return false
}

// DescribeInstances returns instances on this node visible to the caller's account.
func (s *InstanceServiceImpl) DescribeInstances(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error) {
	slog.Info("Processing DescribeInstances request from this node", "accountID", accountID)

	instanceIDFilter := make(map[string]bool)
	for _, id := range input.InstanceIds {
		if id != nil && *id != "" {
			if !strings.HasPrefix(*id, "i-") {
				return nil, errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
			}
			instanceIDFilter[*id] = true
		}
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, DescribeInstancesValidFilters)
	if err != nil {
		slog.Warn("DescribeInstances: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	reservationMap := make(map[string]*ec2.Reservation)

	// Iterate under the manager lock — VM fields (Status, Instance, Reservation,
	// PublicIP, PlacementGroupName) are mutated through manager-locked
	// Inspect/UpdateState elsewhere, so reading them lock-free would race.
	s.vmMgr.View(func(vms map[string]*vm.VM) {
		for _, instance := range vms {
			if !IsInstanceVisible(accountID, instance.AccountID) {
				continue
			}
			if len(instanceIDFilter) > 0 && !instanceIDFilter[instance.ID] {
				continue
			}
			if instance.Reservation == nil || instance.Instance == nil {
				continue
			}

			resID := ""
			if instance.Reservation.ReservationId != nil {
				resID = *instance.Reservation.ReservationId
			}

			if _, exists := reservationMap[resID]; !exists {
				reservation := &ec2.Reservation{}
				reservation.SetReservationId(resID)
				if instance.Reservation.OwnerId != nil {
					reservation.SetOwnerId(*instance.Reservation.OwnerId)
				}
				reservation.Instances = []*ec2.Instance{}
				reservationMap[resID] = reservation
			}

			instanceCopy := *instance.Instance
			instanceCopy.State = &ec2.InstanceState{}

			if instance.PublicIP != "" && instanceCopy.PublicIpAddress == nil {
				instanceCopy.PublicIpAddress = aws.String(instance.PublicIP)
			}

			if info, ok := vm.EC2StateCodes[instance.Status]; ok {
				instanceCopy.State.SetCode(info.Code)
				instanceCopy.State.SetName(info.Name)
			} else {
				slog.Warn("Instance has unmapped status, reporting as pending",
					"instanceId", instance.ID, "status", string(instance.Status))
				instanceCopy.State.SetCode(0)
				instanceCopy.State.SetName("pending")
			}

			if instance.PlacementGroupName != "" {
				instanceCopy.Placement = &ec2.Placement{
					GroupName:        aws.String(instance.PlacementGroupName),
					AvailabilityZone: aws.String(s.config.AZ),
				}
			}

			if len(parsedFilters) > 0 && !instanceMatchesFilters(instance, &instanceCopy, parsedFilters) {
				continue
			}

			reservationMap[resID].Instances = append(reservationMap[resID].Instances, &instanceCopy)
		}
	})

	reservations := make([]*ec2.Reservation, 0, len(reservationMap))
	for _, reservation := range reservationMap {
		reservations = append(reservations, reservation)
	}

	slog.Info("DescribeInstances completed", "count", len(reservations))
	return &ec2.DescribeInstancesOutput{Reservations: reservations}, nil
}

// DescribeInstanceTypes returns instance types provisionable on this node.
// The "capacity" filter (when "true") expands each type to one entry per
// available slot so callers can report cluster-wide capacity by aggregating.
func (s *InstanceServiceImpl) DescribeInstanceTypes(input *ec2.DescribeInstanceTypesInput, _ string) (*ec2.DescribeInstanceTypesOutput, error) {
	slog.Info("Processing DescribeInstanceTypes request from this node")

	if s.resourceMgr == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	showCapacity := false
	for _, f := range input.Filters {
		if f.Name != nil && *f.Name == "capacity" {
			for _, v := range f.Values {
				if v != nil && *v == "true" {
					showCapacity = true
					break
				}
			}
		}
	}

	filteredTypes := s.resourceMgr.GetAvailableInstanceTypeInfos(showCapacity)
	slog.Info("DescribeInstanceTypes completed", "count", len(filteredTypes))
	return &ec2.DescribeInstanceTypesOutput{InstanceTypes: filteredTypes}, nil
}

// DescribeStoppedInstances returns stopped instances from shared KV.
func (s *InstanceServiceImpl) DescribeStoppedInstances(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error) {
	if s.stoppedStore == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return s.describeInstancesFromKV(input, accountID, s.stoppedStore.ListStoppedInstances, 80, "stopped", "DescribeStoppedInstances")
}

// DescribeTerminatedInstances returns terminated instances from the terminated KV bucket.
func (s *InstanceServiceImpl) DescribeTerminatedInstances(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error) {
	if s.stoppedStore == nil {
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return s.describeInstancesFromKV(input, accountID, s.stoppedStore.ListTerminatedInstances, 48, "terminated", "DescribeTerminatedInstances")
}

func (s *InstanceServiceImpl) describeInstancesFromKV(input *ec2.DescribeInstancesInput, accountID string, listFn func() ([]*vm.VM, error), fallbackCode int64, fallbackName, opName string) (*ec2.DescribeInstancesOutput, error) {
	instanceIDFilter := make(map[string]bool)
	for _, id := range input.InstanceIds {
		if id != nil {
			instanceIDFilter[*id] = true
		}
	}

	parsedFilters, filterErr := filterutil.ParseFilters(input.Filters, DescribeInstancesValidFilters)
	if filterErr != nil {
		slog.Warn(opName+": invalid filter", "err", filterErr)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	instances, err := listFn()
	if err != nil {
		slog.Error(opName+": failed to list instances", "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	reservationMap := make(map[string]*ec2.Reservation)

	for _, instance := range instances {
		if !IsInstanceVisible(accountID, instance.AccountID) {
			continue
		}
		if len(instanceIDFilter) > 0 && !instanceIDFilter[instance.ID] {
			continue
		}
		if instance.Reservation == nil || instance.Instance == nil {
			slog.Warn(opName+": skipping instance with nil Reservation/Instance (data integrity issue)",
				"instanceId", instance.ID)
			continue
		}

		resID := ""
		if instance.Reservation.ReservationId != nil {
			resID = *instance.Reservation.ReservationId
		}

		if _, exists := reservationMap[resID]; !exists {
			reservation := &ec2.Reservation{}
			reservation.SetReservationId(resID)
			if instance.Reservation.OwnerId != nil {
				reservation.SetOwnerId(*instance.Reservation.OwnerId)
			}
			reservation.Instances = []*ec2.Instance{}
			reservationMap[resID] = reservation
		}

		instanceCopy := *instance.Instance
		instanceCopy.State = &ec2.InstanceState{}
		if info, ok := vm.EC2StateCodes[instance.Status]; ok {
			instanceCopy.State.SetCode(info.Code)
			instanceCopy.State.SetName(info.Name)
		} else {
			instanceCopy.State.SetCode(fallbackCode)
			instanceCopy.State.SetName(fallbackName)
		}

		if len(parsedFilters) > 0 && !instanceMatchesFilters(instance, &instanceCopy, parsedFilters) {
			continue
		}

		reservationMap[resID].Instances = append(reservationMap[resID].Instances, &instanceCopy)
	}

	reservations := make([]*ec2.Reservation, 0, len(reservationMap))
	for _, reservation := range reservationMap {
		reservations = append(reservations, reservation)
	}

	slog.Info(opName+" completed", "count", len(reservations))
	return &ec2.DescribeInstancesOutput{Reservations: reservations}, nil
}

// ModifyInstanceAttribute applies a single attribute change. InstanceType and
// UserData require the instance to be stopped. DisableApiTermination works on
// both running and stopped instances. SourceDestCheck is a networking concept
// that does not apply to bare-metal VMs; it is accepted as a no-op on any
// instance state so Terraform and the AWS CLI do not error out.
func (s *InstanceServiceImpl) ModifyInstanceAttribute(input *ec2.ModifyInstanceAttributeInput, accountID string) (*ec2.ModifyInstanceAttributeOutput, error) {
	if input.InstanceId == nil || *input.InstanceId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	instanceID := *input.InstanceId

	if input.SourceDestCheck != nil {
		slog.Info("ModifyInstanceAttribute: accepting SourceDestCheck (no-op on bare metal)", "instanceId", instanceID)
		return &ec2.ModifyInstanceAttributeOutput{}, nil
	}

	// DisableApiTermination applies to both running and stopped instances.
	// Try the running map first; fall through to the stopped-store path only
	// if the instance isn't currently running on this node.
	if input.DisableApiTermination != nil && input.DisableApiTermination.Value != nil {
		newVal := input.DisableApiTermination.Value
		var notVisible bool
		updated, persistErr := s.vmMgr.UpdateAndPersist(instanceID, func(v *vm.VM) bool {
			if !IsInstanceVisible(accountID, v.AccountID) {
				notVisible = true
				return false
			}
			if v.RunInstancesInput == nil {
				v.RunInstancesInput = &ec2.RunInstancesInput{}
			}
			v.RunInstancesInput.DisableApiTermination = newVal
			return true
		})
		if notVisible {
			slog.Warn("ModifyInstanceAttribute: instance not visible",
				"instanceId", instanceID, "callerAccount", accountID)
			return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}
		if updated {
			if persistErr != nil {
				slog.Error("ModifyInstanceAttribute: persist failed",
					"instanceId", instanceID, "err", persistErr)
				return nil, errors.New(awserrors.ErrorServerInternal)
			}
			slog.Info("ModifyInstanceAttribute: updated DisableApiTermination on running instance",
				"instanceId", instanceID, "value", *newVal)
			return &ec2.ModifyInstanceAttributeOutput{}, nil
		}
		// Not in running map — fall through to stopped-store path.
	}

	if s.stoppedStore == nil {
		slog.Error("ModifyInstanceAttribute: stopped store not available")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	instance, err := s.stoppedStore.LoadStoppedInstance(instanceID)
	if err != nil {
		slog.Error("ModifyInstanceAttribute: failed to load stopped instance", "instanceId", instanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if instance == nil {
		slog.Warn("ModifyInstanceAttribute: instance not found in shared KV", "instanceId", instanceID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	if instance.Status != vm.StateStopped {
		slog.Error("ModifyInstanceAttribute: instance not in stopped state", "instanceId", instanceID, "status", instance.Status)
		return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
	}

	if !IsInstanceVisible(accountID, instance.AccountID) {
		slog.Warn("ModifyInstanceAttribute: instance not visible",
			"instanceId", instanceID, "callerAccount", accountID, "ownerAccount", instance.AccountID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	if input.InstanceType != nil && input.InstanceType.Value != nil {
		newType := *input.InstanceType.Value
		if newType == "" {
			slog.Error("ModifyInstanceAttribute: empty instance type value", "instanceId", instanceID)
			return nil, errors.New(awserrors.ErrorInvalidInstanceAttributeValue)
		}
		if instance.Instance == nil {
			slog.Error("ModifyInstanceAttribute: instance.Instance is nil, data integrity issue", "instanceId", instanceID)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		slog.Info("ModifyInstanceAttribute: changing instance type",
			"instanceId", instanceID, "oldType", instance.InstanceType, "newType", newType)

		instance.InstanceType = newType
		instance.Config.InstanceType = newType
		instance.Instance.InstanceType = aws.String(newType)
		// Clear StateReason — resolves capacity-unavailable state from instance-type-missing bug.
		instance.Instance.StateReason = nil
	}

	if input.UserData != nil && input.UserData.Value != nil {
		slog.Info("ModifyInstanceAttribute: changing user data", "instanceId", instanceID)
		instance.UserData = string(input.UserData.Value)
		if instance.RunInstancesInput != nil {
			instance.RunInstancesInput.UserData = aws.String(base64.StdEncoding.EncodeToString(input.UserData.Value))
		}
	}

	if input.DisableApiTermination != nil && input.DisableApiTermination.Value != nil {
		slog.Info("ModifyInstanceAttribute: changing DisableApiTermination on stopped instance",
			"instanceId", instanceID, "value", *input.DisableApiTermination.Value)
		if instance.RunInstancesInput == nil {
			instance.RunInstancesInput = &ec2.RunInstancesInput{}
		}
		instance.RunInstancesInput.DisableApiTermination = input.DisableApiTermination.Value
	}

	if err := s.stoppedStore.WriteStoppedInstance(instanceID, instance); err != nil {
		slog.Error("ModifyInstanceAttribute: failed to write modified instance to KV",
			"instanceId", instanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	slog.Info("ModifyInstanceAttribute: completed successfully", "instanceId", instanceID)
	return &ec2.ModifyInstanceAttributeOutput{}, nil
}

// StoppedInstanceNode returns the LastNode field stored in the shared KV for a
// stopped instance. Returns "" when the store is unavailable, the instance is
// not found, or the entry has no LastNode set. Used by the daemon's ec2.start
// handler to route start requests back to the original node when possible.
func (s *InstanceServiceImpl) StoppedInstanceNode(instanceID string) string {
	if s.stoppedStore == nil {
		return ""
	}
	instance, err := s.stoppedStore.LoadStoppedInstance(instanceID)
	if err != nil || instance == nil {
		return ""
	}
	return instance.LastNode
}

// StartStoppedInstance picks up a stopped instance from shared KV, re-launches
// it on this daemon node, then removes it from shared KV.
func (s *InstanceServiceImpl) StartStoppedInstance(input *StartStoppedInstanceInput, accountID string) (*StartStoppedInstanceOutput, error) {
	if input.InstanceID == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if s.stoppedStore == nil {
		slog.Error("StartStoppedInstance: stopped store not available")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if s.resourceMgr == nil {
		slog.Error("StartStoppedInstance: resource manager not available")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if s.vmMgr == nil {
		slog.Error("StartStoppedInstance: vm manager not available")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	instance, err := s.stoppedStore.LoadStoppedInstance(input.InstanceID)
	if err != nil {
		slog.Error("StartStoppedInstance: failed to load stopped instance", "instanceId", input.InstanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if instance == nil {
		slog.Warn("StartStoppedInstance: instance not found in shared KV", "instanceId", input.InstanceID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	if instance.Status != vm.StateStopped {
		slog.Error("StartStoppedInstance: instance not in stopped state", "instanceId", input.InstanceID, "status", instance.Status)
		return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
	}
	if !IsInstanceVisible(accountID, instance.AccountID) {
		slog.Warn("StartStoppedInstance: instance not visible",
			"instanceId", input.InstanceID, "callerAccount", accountID, "ownerAccount", instance.AccountID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	// Reset node-local fields that are stale after cross-node migration.
	instance.ResetNodeLocalState()

	instanceType, ok := s.resourceMgr.InstanceTypes()[instance.InstanceType]
	if !ok {
		slog.Error("StartStoppedInstance: instance type not available on this node",
			"instanceId", input.InstanceID, "instanceType", instance.InstanceType)
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}
	if err := s.resourceMgr.Allocate(instanceType); err != nil {
		slog.Error("StartStoppedInstance: failed to allocate resources", "instanceId", input.InstanceID, "err", err)
		return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
	}

	// Add to local map + clear stop attribute before launch.
	instance.Attributes = spxtypes.EC2CommandAttributes{StartInstance: true}
	s.vmMgr.Insert(instance)

	// Claim GPU for GPU instance types — binds the full IOMMU group to vfio-pci.
	gpuClaimed := false
	if s.gpuClaimer != nil && instancetypes.IsGPUType(instanceType) {
		pciAddr, xvga, gpuErr := s.gpuClaimer.Claim(instance.ID)
		if gpuErr != nil {
			slog.Error("StartStoppedInstance: GPU claim failed", "instanceId", input.InstanceID, "err", gpuErr)
			s.resourceMgr.Deallocate(instanceType)
			s.vmMgr.Delete(instance.ID)
			return nil, errors.New(awserrors.ErrorInsufficientInstanceCapacity)
		}
		instance.GPUPCIAddresses = []string{pciAddr}
		instance.GPUXVGAEnabled = xvga
		gpuClaimed = true
		slog.Info("GPU claimed for instance", "instanceId", input.InstanceID, "gpu", pciAddr, "xvga", xvga)
	}

	if err := s.vmMgr.Run(instance); err != nil {
		slog.Error("StartStoppedInstance: vmMgr.Run failed", "instanceId", input.InstanceID, "err", err)
		if gpuClaimed {
			if relErr := s.gpuClaimer.Release(instance.ID); relErr != nil {
				slog.Error("StartStoppedInstance: GPU release failed after launch failure",
					"instanceId", input.InstanceID, "err", relErr)
			}
		}
		s.resourceMgr.Deallocate(instanceType)
		s.vmMgr.Delete(instance.ID)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Discover actual guest device names via QMP query-block.
	s.vmMgr.UpdateGuestDeviceNames(instance)

	// Remove from shared KV now that it's running locally. Retry once on failure —
	// a stale KV entry risks duplicate starts.
	if err := s.stoppedStore.DeleteStoppedInstance(input.InstanceID); err != nil {
		slog.Warn("StartStoppedInstance: first KV delete failed, retrying",
			"instanceId", input.InstanceID, "err", err)
		if retryErr := s.stoppedStore.DeleteStoppedInstance(input.InstanceID); retryErr != nil {
			slog.Error("StartStoppedInstance: KV delete failed after retry, instance is running locally but stale entry remains in shared KV",
				"instanceId", input.InstanceID, "err", retryErr)
		}
	}

	slog.Info("Started stopped instance from shared KV", "instanceId", instance.ID)
	return &StartStoppedInstanceOutput{Status: "running", InstanceID: instance.ID}, nil
}

// TerminateStoppedInstance picks up a stopped instance from shared KV, deletes
// its volumes, releases its public IP and ENI, writes it to the terminated
// bucket, then removes it from the stopped bucket. No QEMU shutdown or unmount
// is needed — the instance was already drained during stop.
func (s *InstanceServiceImpl) TerminateStoppedInstance(input *TerminateStoppedInstanceInput, accountID string) (*TerminateStoppedInstanceOutput, error) {
	if input.InstanceID == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if s.stoppedStore == nil {
		slog.Error("TerminateStoppedInstance: stopped store not available")
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	instance, err := s.stoppedStore.LoadStoppedInstance(input.InstanceID)
	if err != nil {
		slog.Error("TerminateStoppedInstance: failed to load stopped instance", "instanceId", input.InstanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if instance == nil {
		slog.Warn("TerminateStoppedInstance: instance not found in shared KV", "instanceId", input.InstanceID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	if instance.Status != vm.StateStopped {
		slog.Error("TerminateStoppedInstance: instance not in stopped state", "instanceId", input.InstanceID, "status", instance.Status)
		return nil, errors.New(awserrors.ErrorIncorrectInstanceState)
	}
	if !IsInstanceVisible(accountID, instance.AccountID) {
		slog.Warn("TerminateStoppedInstance: instance not visible",
			"instanceId", input.InstanceID, "callerAccount", accountID, "ownerAccount", instance.AccountID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	if instance.IsTerminationProtected() {
		slog.Warn("TerminateStoppedInstance: instance has termination protection",
			"instanceId", input.InstanceID)
		return nil, errors.New(awserrors.ErrorOperationNotPermitted)
	}

	s.deleteInstanceVolumes(instance, input.InstanceID)
	s.releaseInstancePublicIP(instance, input.InstanceID)
	s.deleteInstanceENI(instance, input.InstanceID)

	// Write to terminated KV FIRST so the instance is visible in DescribeInstances.
	// If this fails the instance stays in the stopped bucket — safe to retry.
	instance.Status = vm.StateTerminated
	if err := s.stoppedStore.WriteTerminatedInstance(input.InstanceID, instance); err != nil {
		slog.Error("TerminateStoppedInstance: failed to write to terminated KV, aborting", "instanceId", input.InstanceID, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}

	// Now safe to remove from stopped KV. Retry once on failure so the instance
	// doesn't appear in both buckets.
	if err := s.stoppedStore.DeleteStoppedInstance(input.InstanceID); err != nil {
		slog.Warn("TerminateStoppedInstance: first stopped KV delete failed, retrying",
			"instanceId", input.InstanceID, "err", err)
		if retryErr := s.stoppedStore.DeleteStoppedInstance(input.InstanceID); retryErr != nil {
			slog.Error("TerminateStoppedInstance: stopped KV delete failed after retry, instance may appear in both buckets",
				"instanceId", input.InstanceID, "err", retryErr)
		}
	}

	slog.Info("Terminated stopped instance from shared KV", "instanceId", input.InstanceID)
	return &TerminateStoppedInstanceOutput{Status: "terminated", InstanceID: input.InstanceID}, nil
}

func (s *InstanceServiceImpl) deleteInstanceVolumes(instance *vm.VM, instanceID string) {
	instance.EBSRequests.Mu.Lock()
	defer instance.EBSRequests.Mu.Unlock()
	for _, ebsRequest := range instance.EBSRequests.Requests {
		// Internal volumes (EFI, cloud-init) are always cleaned up via ebs.delete.
		if ebsRequest.EFI || ebsRequest.CloudInit {
			ebsDeleteData, err := json.Marshal(spxtypes.EBSDeleteRequest{Volume: ebsRequest.Name})
			if err != nil {
				slog.Error("TerminateStoppedInstance: failed to marshal ebs.delete request", "name", ebsRequest.Name, "err", err)
				continue
			}
			deleteMsg, err := s.natsConn.Request("ebs.delete", ebsDeleteData, 30*time.Second)
			if err != nil {
				slog.Warn("TerminateStoppedInstance: ebs.delete failed for internal volume", "name", ebsRequest.Name, "err", err)
			} else {
				slog.Info("TerminateStoppedInstance: ebs.delete sent for internal volume", "name", ebsRequest.Name, "data", string(deleteMsg.Data))
			}
			continue
		}

		// User-visible volumes: respect DeleteOnTermination.
		if !ebsRequest.DeleteOnTermination {
			slog.Info("TerminateStoppedInstance: volume has DeleteOnTermination=false, skipping", "name", ebsRequest.Name)
			continue
		}

		slog.Info("TerminateStoppedInstance: deleting volume with DeleteOnTermination=true", "name", ebsRequest.Name)
		if s.volumeDeleter == nil {
			slog.Warn("TerminateStoppedInstance: volume deleter not configured, skipping", "name", ebsRequest.Name)
			continue
		}
		if _, err := s.volumeDeleter.DeleteVolume(&ec2.DeleteVolumeInput{
			VolumeId: &ebsRequest.Name,
		}, instance.AccountID); err != nil {
			slog.Error("TerminateStoppedInstance: failed to delete volume", "name", ebsRequest.Name, "err", err)
		}
	}
	_ = instanceID
}

func (s *InstanceServiceImpl) releaseInstancePublicIP(instance *vm.VM, instanceID string) {
	if instance.PublicIP == "" || instance.PublicIPPool == "" || s.ipReleaser == nil {
		return
	}
	portName := "port-" + instance.ENIId
	vpcID := ""
	logicalIP := ""
	if instance.Instance != nil {
		if instance.Instance.VpcId != nil {
			vpcID = *instance.Instance.VpcId
		}
		if instance.Instance.PrivateIpAddress != nil {
			logicalIP = *instance.Instance.PrivateIpAddress
		}
	}
	utils.PublishNATEvent(s.natsConn, "vpc.delete-nat", vpcID, instance.PublicIP, logicalIP, portName, "")

	if err := s.ipReleaser.ReleaseIP(instance.PublicIPPool, instance.PublicIP); err != nil {
		slog.Warn("TerminateStoppedInstance: failed to release public IP", "ip", instance.PublicIP, "pool", instance.PublicIPPool, "err", err)
	} else {
		slog.Info("TerminateStoppedInstance: released public IP", "ip", instance.PublicIP, "instanceId", instanceID)
	}
}

func (s *InstanceServiceImpl) deleteInstanceENI(instance *vm.VM, instanceID string) {
	if instance.ENIId == "" || s.eniDeleter == nil {
		return
	}
	if _, err := s.eniDeleter.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: &instance.ENIId,
	}, instance.AccountID); err != nil {
		slog.Error("TerminateStoppedInstance: failed to delete ENI", "eni", instance.ENIId, "err", err)
	} else {
		slog.Info("TerminateStoppedInstance: deleted ENI", "eni", instance.ENIId, "instanceId", instanceID)
	}
}

// DescribeInstanceAttribute returns a single requested attribute for an instance.
// Checks running instances first (in-memory), then falls back to stopped instances in KV.
func (s *InstanceServiceImpl) DescribeInstanceAttribute(input *ec2.DescribeInstanceAttributeInput, accountID string) (*ec2.DescribeInstanceAttributeOutput, error) {
	if input.InstanceId == nil || *input.InstanceId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.Attribute == nil || *input.Attribute == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	instanceID := *input.InstanceId
	attribute := *input.Attribute

	var instance *vm.VM
	if running, ok := s.vmMgr.Get(instanceID); ok {
		instance = running
	}

	if instance == nil {
		if s.stoppedStore == nil {
			slog.Error("DescribeInstanceAttribute: stopped store not available")
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		stopped, err := s.stoppedStore.LoadStoppedInstance(instanceID)
		if err != nil {
			slog.Error("DescribeInstanceAttribute: failed to load stopped instance",
				"instanceId", instanceID, "err", err)
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		instance = stopped
	}

	if instance == nil {
		slog.Warn("DescribeInstanceAttribute: instance not found",
			"instanceId", instanceID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	if !IsInstanceVisible(accountID, instance.AccountID) {
		slog.Warn("DescribeInstanceAttribute: instance not visible",
			"instanceId", instanceID, "callerAccount", accountID, "ownerAccount", instance.AccountID)
		return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}

	output := &ec2.DescribeInstanceAttributeOutput{
		InstanceId: &instanceID,
	}

	switch attribute {
	case ec2.InstanceAttributeNameInstanceType:
		val := instance.InstanceType
		output.InstanceType = &ec2.AttributeValue{Value: &val}

	case ec2.InstanceAttributeNameUserData:
		val := instance.UserData
		output.UserData = &ec2.AttributeValue{Value: &val}

	case ec2.InstanceAttributeNameDisableApiTermination:
		// Read under the manager lock so we serialise with a concurrent
		// ModifyInstanceAttribute writer. Inspect is a no-op race-wise for
		// stopped instances (no concurrent writers) but keeps the call site
		// uniform.
		var val bool
		s.vmMgr.Inspect(instance, func(v *vm.VM) {
			val = v.IsTerminationProtected()
		})
		output.DisableApiTermination = &ec2.AttributeBooleanValue{Value: &val}

	case ec2.InstanceAttributeNameDisableApiStop:
		val := false
		output.DisableApiStop = &ec2.AttributeBooleanValue{Value: &val}

	case ec2.InstanceAttributeNameInstanceInitiatedShutdownBehavior:
		val := ec2.ShutdownBehaviorStop
		output.InstanceInitiatedShutdownBehavior = &ec2.AttributeValue{Value: &val}

	case ec2.InstanceAttributeNameEbsOptimized:
		val := false
		output.EbsOptimized = &ec2.AttributeBooleanValue{Value: &val}

	case ec2.InstanceAttributeNameEnaSupport:
		val := true
		output.EnaSupport = &ec2.AttributeBooleanValue{Value: &val}

	case ec2.InstanceAttributeNameSourceDestCheck:
		val := true
		output.SourceDestCheck = &ec2.AttributeBooleanValue{Value: &val}

	case ec2.InstanceAttributeNameGroupSet:
		if instance.Instance != nil && len(instance.Instance.SecurityGroups) > 0 {
			output.Groups = instance.Instance.SecurityGroups
		} else {
			output.Groups = []*ec2.GroupIdentifier{}
		}

	default:
		slog.Warn("DescribeInstanceAttribute: unsupported attribute",
			"instanceId", instanceID, "attribute", attribute)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	slog.Info("DescribeInstanceAttribute: completed",
		"instanceId", instanceID, "attribute", attribute, "accountID", accountID)
	return output, nil
}

// Terminated and error states are deliberately never surfaced: terminated
// matches AWS (drops from DescribeInstanceStatus shortly after termination);
// error is a Spinifex-internal state whose name is not a valid AWS state label.
var (
	describeInstanceStatusRunningOnly = map[vm.InstanceState]bool{vm.StateRunning: true}
	describeInstanceStatusAllIncluded = map[vm.InstanceState]bool{
		vm.StateRunning:      true,
		vm.StatePending:      true,
		vm.StateProvisioning: true,
		vm.StateStopping:     true,
		vm.StateStopped:      true,
		vm.StateShuttingDown: true,
	}
)

func (s *InstanceServiceImpl) buildInstanceStatus(v *vm.VM) *ec2.InstanceStatus {
	state := &ec2.InstanceState{}
	if info, ok := vm.EC2StateCodes[v.Status]; ok {
		state.SetCode(info.Code)
		state.SetName(info.Name)
	} else {
		state.SetCode(vm.EC2StateCodes[vm.StatePending].Code)
		state.SetName(vm.EC2StateCodes[vm.StatePending].Name)
	}

	status := instanceStatusNotApplicable
	reachability := instanceStatusNotApplicable
	if v.Status == vm.StateRunning {
		status = instanceStatusOK
		reachability = instanceStatusPassed
	}

	return &ec2.InstanceStatus{
		AvailabilityZone: aws.String(s.config.AZ),
		InstanceId:       aws.String(v.ID),
		InstanceState:    state,
		InstanceStatus: &ec2.InstanceStatusSummary{
			Status: aws.String(status),
			Details: []*ec2.InstanceStatusDetails{{
				Name:   aws.String(reachabilityDetailName),
				Status: aws.String(reachability),
			}},
		},
		SystemStatus: &ec2.InstanceStatusSummary{
			Status: aws.String(status),
			Details: []*ec2.InstanceStatusDetails{{
				Name:   aws.String(reachabilityDetailName),
				Status: aws.String(reachability),
			}},
		},
	}
}

const (
	instanceStatusOK            = "ok"
	instanceStatusPassed        = "passed"
	instanceStatusNotApplicable = "not-applicable"
	reachabilityDetailName      = "reachability"
)

func instanceStatusMatchesFilters(v *vm.VM, is *ec2.InstanceStatus, filters map[string][]string) bool {
	for name, values := range filters {
		if strings.HasPrefix(name, "tag:") {
			continue
		}
		var field string
		switch name {
		case "availability-zone":
			if is.AvailabilityZone != nil {
				field = *is.AvailabilityZone
			}
		case "instance-state-name":
			if is.InstanceState != nil && is.InstanceState.Name != nil {
				field = *is.InstanceState.Name
			}
		case "instance-state-code":
			if is.InstanceState != nil && is.InstanceState.Code != nil {
				field = strconv.FormatInt(*is.InstanceState.Code, 10)
			}
		default:
			return false
		}
		if !filterutil.MatchesAny(values, field) {
			return false
		}
	}

	if v.Instance != nil {
		tags := filterutil.EC2TagsToMap(v.Instance.Tags)
		return filterutil.MatchesTags(filters, tags)
	}
	return filterutil.MatchesTags(filters, nil)
}

// DescribeInstanceStatus returns per-VM status entries for VMs on this node
// visible to the caller. Stopped instances come from the gateway's KV query,
// not this handler.
func (s *InstanceServiceImpl) DescribeInstanceStatus(input *ec2.DescribeInstanceStatusInput, accountID string) (*ec2.DescribeInstanceStatusOutput, error) {
	slog.Info("Processing DescribeInstanceStatus request from this node", "accountID", accountID)

	instanceIDFilter := make(map[string]bool)
	for _, id := range input.InstanceIds {
		if id == nil || *id == "" {
			continue
		}
		if !strings.HasPrefix(*id, "i-") {
			return nil, errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
		}
		instanceIDFilter[*id] = true
	}

	parsedFilters, err := filterutil.ParseFilters(input.Filters, DescribeInstanceStatusValidFilters)
	if err != nil {
		slog.Warn("DescribeInstanceStatus: invalid filter", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	includedStates := describeInstanceStatusRunningOnly
	if aws.BoolValue(input.IncludeAllInstances) {
		includedStates = describeInstanceStatusAllIncluded
	}

	var statuses []*ec2.InstanceStatus
	s.vmMgr.View(func(vms map[string]*vm.VM) {
		for _, v := range vms {
			if !IsInstanceVisible(accountID, v.AccountID) {
				continue
			}
			if len(instanceIDFilter) > 0 && !instanceIDFilter[v.ID] {
				continue
			}
			if !includedStates[v.Status] {
				continue
			}
			is := s.buildInstanceStatus(v)
			if len(parsedFilters) > 0 && !instanceStatusMatchesFilters(v, is, parsedFilters) {
				continue
			}
			statuses = append(statuses, is)
		}
	})

	slog.Info("DescribeInstanceStatus completed", "count", len(statuses))
	return &ec2.DescribeInstanceStatusOutput{InstanceStatuses: statuses}, nil
}
