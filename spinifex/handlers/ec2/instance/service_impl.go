package handlers_ec2_instance

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
      - sudo
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

{{if .UserDataScript}}
write_files:
  - path: /tmp/cloud-init-startup.sh
    permissions: '0755'
    content: |
{{.UserDataScript}}

runcmd:
  - [ "/bin/bash", "/tmp/cloud-init-startup.sh" ]
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
		// Route for multi-node is added via bootcmd in lbVMUserData (Alpine
		// cloud-init does not support v2 routes under ethernets).
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
	resourceMgr   ResourceCapacityProvider
	stoppedStore  StoppedInstanceStore
}

// NewInstanceServiceImpl creates a new instance service implementation for daemon use
func NewInstanceServiceImpl(
	cfg *config.Config,
	instanceTypes map[string]*ec2.InstanceTypeInfo,
	nc *nats.Conn,
	store objectstore.ObjectStore,
	vmMgr *vm.Manager,
	resourceMgr ResourceCapacityProvider,
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

	// Step 2: Create EFI partition
	err = s.prepareEFIVolume(imageId, volumeConfig, instance)
	if err != nil {
		return nil, err
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

	return viperblock.New(&vbconfig, "s3", cfg)
}

// prepareRootVolume handles creation/cloning of the root volume
func (s *InstanceServiceImpl) prepareRootVolume(input *ec2.RunInstancesInput, imageId string, size int, volumeConfig viperblock.VolumeConfig, instance *vm.VM, deleteOnTermination bool) error {
	vb, err := s.newViperblock(imageId, size, volumeConfig)
	if err != nil {
		slog.Error("Failed to connect to Viperblock store", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	vb.SetDebug(false)

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

// prepareEFIVolume creates the EFI boot partition
func (s *InstanceServiceImpl) prepareEFIVolume(imageId string, volumeConfig viperblock.VolumeConfig, instance *vm.VM) error {
	efiVolumeName := fmt.Sprintf("%s-efi", imageId)
	efiSize := 64 * 1024 * 1024 // 64MB

	// Update VolumeID to match the EFI volume name
	efiVolumeConfig := volumeConfig
	efiVolumeConfig.VolumeMetadata.VolumeID = efiVolumeName

	efiVb, err := s.newViperblock(efiVolumeName, efiSize, efiVolumeConfig)
	if err != nil {
		slog.Error("Could not create EFI viperblock", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	efiVb.SetDebug(false)

	// Initialize the backend
	slog.Debug("Initializing EFI Viperblock store backend")
	err = efiVb.Backend.Init()
	if err != nil {
		slog.Error("Failed to initialize EFI Viperblock store backend", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// Load the state from the remote backend
	_, err = efiVb.LoadStateRequest("")
	slog.Info("LoadStateRequest", "error", err)

	// Create EFI volume if it doesn't exist
	if err != nil {
		slog.Info("Volume does not yet exist, creating EFI disk ...")

		// Open the chunk WAL (sharded or legacy)
		if efiVb.UseShardedWAL {
			err = efiVb.OpenShardedWAL()
		} else {
			err = efiVb.OpenWAL(&efiVb.WAL, fmt.Sprintf("%s/%s", efiVb.WAL.BaseDir, types.GetFilePath(types.FileTypeWALChunk, efiVb.WAL.WallNum.Load(), efiVb.GetVolume())))
		}
		if err != nil {
			slog.Error("Failed to load WAL", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}

		// Open the block to object WAL
		err = efiVb.OpenWAL(&efiVb.BlockToObjectWAL, fmt.Sprintf("%s/%s", efiVb.WAL.BaseDir, types.GetFilePath(types.FileTypeWALBlock, efiVb.BlockToObjectWAL.WallNum.Load(), efiVb.GetVolume())))
		if err != nil {
			slog.Error("Failed to load block WAL", "err", err)
			return errors.New(awserrors.ErrorServerInternal)
		}

		// Write an empty block to the EFI volume
		if err := efiVb.WriteAt(0, make([]byte, efiVb.BlockSize)); err != nil {
			slog.Error("Failed to write empty EFI block", "err", err)
		}
		if err := efiVb.Flush(); err != nil {
			slog.Error("Failed to flush EFI volume", "err", err)
		}
	}

	slog.Info("Closing EFI")
	err = efiVb.Close()
	slog.Info("Close", "error", err)
	if err != nil {
		slog.Error("Failed to close EFI Viperblock store", "err", err)
	}

	err = efiVb.RemoveLocalFiles()
	if err != nil {
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

	cloudInitVb.SetDebug(false)

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

	userData := CloudInitData{
		Username:  "ec2-user",
		SSHKey:    string(sshKey),
		Hostname:  hostname,
		CACertPEM: caCertPEM,
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
	extraMACs := make([]string, 0, len(instance.ExtraENIs))
	for _, extra := range instance.ExtraENIs {
		extraMACs = append(extraMACs, extra.ENIMac)
	}
	networkConfig := generateNetworkConfig(instance.ENIMac, instance.DevMAC, instance.MgmtMAC, instance.MgmtIP, extraMACs)
	err = writer.AddFile(strings.NewReader(networkConfig), "network-config")
	if err != nil {
		slog.Error("failed to add network-config file", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
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
		val := false
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
