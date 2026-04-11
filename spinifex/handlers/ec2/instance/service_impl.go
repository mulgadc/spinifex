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
	instances     *vm.Instances
	objectStore   objectstore.ObjectStore
}

// NewInstanceServiceImpl creates a new instance service implementation for daemon use
func NewInstanceServiceImpl(cfg *config.Config, instanceTypes map[string]*ec2.InstanceTypeInfo, nc *nats.Conn, instances *vm.Instances, store objectstore.ObjectStore) *InstanceServiceImpl {
	return &InstanceServiceImpl{
		config:        cfg,
		instanceTypes: instanceTypes,
		natsConn:      nc,
		instances:     instances,
		objectStore:   store,
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
