package daemon

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// Compile-time check that Daemon implements SystemInstanceLauncher.
var _ handlers_elbv2.SystemInstanceLauncher = (*Daemon)(nil)

// LaunchSystemInstance creates and starts a system-managed VM. The VM is owned
// by the system account (GlobalAccountID) and is not visible to customer
// DescribeInstances calls.
//
// This is the internal equivalent of RunInstances — it reuses the same
// instance service, resource manager, volume preparation, and QEMU launch
// path, but skips the NATS request/response envelope and key pair validation.
func (d *Daemon) LaunchSystemInstance(input *handlers_elbv2.SystemInstanceInput) (*handlers_elbv2.SystemInstanceOutput, error) {
	accountID := utils.GlobalAccountID
	// ENI account may differ from system account — the ENI is created under
	// the caller's account, so lookups/updates must use that account ID.
	eniAccountID := input.AccountID
	if eniAccountID == "" {
		eniAccountID = accountID
	}

	// Validate instance type
	instanceType, exists := d.resourceMgr.instanceTypes[input.InstanceType]
	if !exists {
		return nil, fmt.Errorf("unknown instance type: %s", input.InstanceType)
	}

	// Validate AMI
	if d.imageService == nil {
		return nil, fmt.Errorf("image service not initialized")
	}
	if _, err := d.imageService.GetAMIConfig(input.ImageID); err != nil {
		return nil, fmt.Errorf("AMI %s not found: %w", input.ImageID, err)
	}

	// Allocate resources
	if err := d.resourceMgr.allocate(instanceType); err != nil {
		return nil, fmt.Errorf("insufficient capacity for %s: %w", input.InstanceType, err)
	}

	// Build a RunInstancesInput for the instance service
	runInput := &ec2.RunInstancesInput{
		InstanceType: aws.String(input.InstanceType),
		ImageId:      aws.String(input.ImageID),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}
	if input.SubnetID != "" {
		runInput.SubnetId = aws.String(input.SubnetID)
	}
	if input.UserData != "" {
		encoded := base64.StdEncoding.EncodeToString([]byte(input.UserData))
		runInput.UserData = aws.String(encoded)
	}

	// Create VM via instance service
	instance, ec2Instance, err := d.instanceService.RunInstance(runInput)
	if err != nil {
		d.resourceMgr.deallocate(instanceType)
		return nil, fmt.Errorf("create instance: %w", err)
	}

	instance.AccountID = accountID
	instance.Reservation = &ec2.Reservation{}
	instance.Reservation.SetReservationId(utils.GenerateResourceID("r"))
	instance.Reservation.SetOwnerId(accountID)
	instance.Reservation.Instances = []*ec2.Instance{ec2Instance}

	// Attach ENI — either use pre-created one or auto-create
	privateIP := ""
	if input.ENIID != "" {
		// Use pre-created ENI (e.g. ALB ENI)
		instance.ENIId = input.ENIID
		instance.ENIMac = input.ENIMac
		privateIP = input.ENIIP
		ec2Instance.SetPrivateIpAddress(privateIP)
		if input.SubnetID != "" {
			ec2Instance.SetSubnetId(input.SubnetID)
		}
		// Mark ENI as attached to this instance
		if d.vpcService != nil {
			if _, attachErr := d.vpcService.AttachENI(eniAccountID, instance.ENIId, instance.ID, 0); attachErr != nil {
				slog.Warn("LaunchSystemInstance: failed to attach ENI", "eniId", instance.ENIId, "instanceId", instance.ID, "err", attachErr)
			}
		}
		// Attach any additional pre-created ENIs (multi-subnet ALB VMs).
		// Use the launcher input slice as the source of truth so the VM
		// record carries every NIC the ALB spans even if the attach fails
		// for one of them (we clean up on failure below).
		for idx, extra := range input.ExtraENIs {
			if d.vpcService != nil {
				if _, attachErr := d.vpcService.AttachENI(eniAccountID, extra.ENIID, instance.ID, int64(idx+1)); attachErr != nil {
					slog.Error("LaunchSystemInstance: failed to attach extra ENI", "eniId", extra.ENIID, "instanceId", instance.ID, "err", attachErr)
					d.cleanupFailedSystemInstance(instance, instanceType)
					return nil, fmt.Errorf("attach extra ENI %s: %w", extra.ENIID, attachErr)
				}
			}
			instance.ExtraENIs = append(instance.ExtraENIs, vm.ExtraENI{
				ENIID:    extra.ENIID,
				ENIMac:   extra.ENIMac,
				ENIIP:    extra.ENIIP,
				SubnetID: extra.SubnetID,
			})
		}
	} else if input.SubnetID != "" && d.vpcService != nil {
		// Auto-create ENI in subnet
		eniOut, eniErr := d.vpcService.CreateNetworkInterface(&ec2.CreateNetworkInterfaceInput{
			SubnetId:    aws.String(input.SubnetID),
			Description: aws.String("System interface for " + instance.ID),
		}, accountID)
		if eniErr != nil {
			d.resourceMgr.deallocate(instanceType)
			return nil, fmt.Errorf("create ENI: %w", eniErr)
		}
		eni := eniOut.NetworkInterface
		instance.ENIId = *eni.NetworkInterfaceId
		instance.ENIMac = *eni.MacAddress
		privateIP = *eni.PrivateIpAddress
		ec2Instance.SetPrivateIpAddress(privateIP)
		ec2Instance.SetSubnetId(input.SubnetID)
		ec2Instance.SetVpcId(*eni.VpcId)
		// Mark ENI as attached
		if _, attachErr := d.vpcService.AttachENI(accountID, instance.ENIId, instance.ID, 0); attachErr != nil {
			slog.Warn("LaunchSystemInstance: failed to attach auto-created ENI", "eniId", instance.ENIId, "instanceId", instance.ID, "err", attachErr)
		}
	}

	// Allocate public IP for internet-facing ALBs
	publicIP := ""
	if input.Scheme == handlers_elbv2.SchemeInternetFacing && d.externalIPAM != nil && d.vpcService != nil && instance.ENIId != "" {
		region := ""
		az := ""
		if d.config != nil {
			region = d.config.Region
			az = d.config.AZ
		}
		allocatedIP, poolName, allocErr := d.externalIPAM.AllocateIP(region, az, "auto_assign", instance.ENIId, instance.ID)
		if allocErr != nil {
			slog.Error("LaunchSystemInstance: failed to allocate public IP for internet-facing ALB", "instanceId", instance.ID, "err", allocErr)
			d.cleanupFailedSystemInstance(instance, instanceType)
			return nil, fmt.Errorf("allocate public IP for internet-facing ALB: %w", allocErr)
		} else {
			publicIP = allocatedIP
			if updateErr := d.vpcService.UpdateENIPublicIP(eniAccountID, instance.ENIId, publicIP, poolName); updateErr != nil {
				slog.Warn("LaunchSystemInstance: failed to update ENI with public IP", "eniId", instance.ENIId, "err", updateErr)
			}
			// Look up VpcId from the ENI for the NAT event
			vpcID := ""
			result, descErr := d.vpcService.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
				NetworkInterfaceIds: []*string{aws.String(instance.ENIId)},
			}, eniAccountID)
			if descErr == nil && len(result.NetworkInterfaces) > 0 && result.NetworkInterfaces[0].VpcId != nil {
				vpcID = *result.NetworkInterfaces[0].VpcId
			}
			portName := "port-" + instance.ENIId
			d.publishNATEvent("vpc.add-nat", vpcID, publicIP, privateIP, portName, instance.ENIMac)
			instance.PublicIP = publicIP
			instance.PublicIPPool = poolName
			slog.Info("LaunchSystemInstance: allocated public IP",
				"instanceId", instance.ID,
				"publicIp", publicIP,
				"pool", poolName,
			)
		}
	}

	// Add to daemon state so LaunchInstance can find it
	d.Instances.Mu.Lock()
	d.Instances.VMS[instance.ID] = instance
	d.Instances.Mu.Unlock()

	if err := d.WriteState(); err != nil {
		slog.Warn("LaunchSystemInstance: failed to write state", "instanceId", instance.ID, "err", err)
	}

	// Subscribe to per-instance NATS topic for terminate commands
	d.mu.Lock()
	sub, subErr := d.natsConn.Subscribe(fmt.Sprintf("ec2.cmd.%s", instance.ID), d.handleEC2Events)
	if subErr != nil {
		slog.Warn("LaunchSystemInstance: failed to subscribe to instance topic", "instanceId", instance.ID, "err", subErr)
	} else {
		d.natsSubscriptions[instance.ID] = sub
	}
	d.mu.Unlock()

	// Pre-compute dev MAC for dual-NIC cloud-init
	if d.config.Daemon.DevNetworking && instance.ENIId != "" {
		instance.DevMAC = generateDevMAC(instance.ID)
	}

	// Management NIC: allocate IP, generate MAC, create TAP on br-mgmt
	if d.mgmtIPAllocator != nil && d.mgmtBridgeIP != "" {
		mgmtBridge := "br-mgmt"
		if d.config.Daemon.MgmtBridge != "" {
			mgmtBridge = d.config.Daemon.MgmtBridge
		}

		mgmtIP, allocErr := d.mgmtIPAllocator.Allocate(instance.ID)
		if allocErr != nil {
			if input.Scheme == handlers_elbv2.SchemeInternal {
				d.cleanupFailedSystemInstance(instance, instanceType)
				return nil, fmt.Errorf("allocate mgmt IP for internal-scheme ALB: %w", allocErr)
			}
			slog.Warn("LaunchSystemInstance: failed to allocate mgmt IP, skipping mgmt NIC", "instanceId", instance.ID, "err", allocErr)
		} else {
			instance.MgmtMAC = generateMgmtMAC(instance.ID)
			instance.MgmtIP = mgmtIP

			tapName, tapErr := SetupMgmtTapDevice(instance.ID, instance.MgmtMAC, mgmtBridge)
			if tapErr != nil {
				d.mgmtIPAllocator.Release(instance.ID)
				instance.MgmtMAC = ""
				instance.MgmtIP = ""
				if input.Scheme == handlers_elbv2.SchemeInternal {
					d.cleanupFailedSystemInstance(instance, instanceType)
					return nil, fmt.Errorf("setup mgmt tap for internal-scheme ALB: %w", tapErr)
				}
				slog.Error("LaunchSystemInstance: failed to setup mgmt tap", "instanceId", instance.ID, "err", tapErr)
			} else {
				instance.MgmtTap = tapName
				slog.Info("LaunchSystemInstance: mgmt NIC configured",
					"instanceId", instance.ID, "mgmtIP", mgmtIP, "mgmtMAC", instance.MgmtMAC, "mgmtTap", tapName)
			}
		}
	}

	// Store extra hostfwd ports (filled in by StartInstance with actual host ports)
	if len(input.HostfwdPorts) > 0 {
		instance.ExtraHostfwd = make(map[int]int, len(input.HostfwdPorts))
		for _, port := range input.HostfwdPorts {
			instance.ExtraHostfwd[port] = 0 // host port assigned in StartInstance
		}
	}

	// Prepare volumes (root volume clone from AMI, cloud-init ISO, etc.)
	volumeInfos, err := d.instanceService.GenerateVolumes(runInput, instance)
	if err != nil {
		d.cleanupFailedSystemInstance(instance, instanceType)
		return nil, fmt.Errorf("generate volumes: %w", err)
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

	// Launch QEMU VM
	if err := d.LaunchInstance(instance); err != nil {
		d.cleanupFailedSystemInstance(instance, instanceType)
		return nil, fmt.Errorf("launch instance: %w", err)
	}

	slog.Info("LaunchSystemInstance completed",
		"instanceId", instance.ID,
		"instanceType", input.InstanceType,
		"privateIp", privateIP,
		"publicIp", publicIP,
		"scheme", input.Scheme,
	)

	return &handlers_elbv2.SystemInstanceOutput{
		InstanceID: instance.ID,
		PrivateIP:  privateIP,
		PublicIP:   publicIP,
		HostfwdMap: instance.ExtraHostfwd,
	}, nil
}

// TerminateSystemInstance stops and cleans up a system-managed VM.
func (d *Daemon) TerminateSystemInstance(instanceID string) error {
	d.Instances.Mu.Lock()
	instance, exists := d.Instances.VMS[instanceID]
	d.Instances.Mu.Unlock()

	if !exists {
		return fmt.Errorf("instance %s not found", instanceID)
	}

	// Transition to shutting-down
	if err := d.TransitionState(instance, vm.StateShuttingDown); err != nil {
		return fmt.Errorf("transition to shutting-down: %w", err)
	}

	// Stop the VM (QEMU shutdown, volume unmount, tap cleanup)
	if err := d.stopInstance(map[string]*vm.VM{instanceID: instance}, true); err != nil {
		slog.Error("TerminateSystemInstance: stopInstance failed", "instanceId", instanceID, "err", err)
		return fmt.Errorf("stop instance: %w", err)
	}

	if err := d.TransitionState(instance, vm.StateTerminated); err != nil {
		slog.Warn("TerminateSystemInstance: failed to transition to terminated", "instanceId", instanceID, "err", err)
	}

	// Write to terminated KV bucket (auto-expires)
	if d.jsManager != nil {
		if err := d.jsManager.WriteTerminatedInstance(instanceID, instance); err != nil {
			slog.Warn("TerminateSystemInstance: failed to write terminated state", "instanceId", instanceID, "err", err)
		}
	}

	// Clean up local state
	d.Instances.Mu.Lock()
	delete(d.Instances.VMS, instanceID)
	d.Instances.Mu.Unlock()

	// Unsubscribe from per-instance NATS topic
	d.mu.Lock()
	if sub, ok := d.natsSubscriptions[instanceID]; ok {
		if err := sub.Unsubscribe(); err != nil {
			slog.Warn("TerminateSystemInstance: failed to unsubscribe", "instanceId", instanceID, "err", err)
		}
		delete(d.natsSubscriptions, instanceID)
	}
	d.mu.Unlock()

	// Deallocate resources
	if it, ok := d.resourceMgr.instanceTypes[instance.InstanceType]; ok {
		d.resourceMgr.deallocate(it)
	}

	slog.Info("TerminateSystemInstance completed", "instanceId", instanceID)
	return nil
}

// cleanupFailedSystemInstance handles cleanup when a system instance launch fails
// after partial setup (state added, volumes created, etc).
func (d *Daemon) cleanupFailedSystemInstance(instance *vm.VM, instanceType *ec2.InstanceTypeInfo) {
	d.markInstanceFailed(instance, "system_instance_launch_failed")
	d.resourceMgr.deallocate(instanceType)

	// Clean up management TAP and release IP
	if instance.MgmtTap != "" {
		if err := CleanupMgmtTapDevice(instance.MgmtTap); err != nil {
			slog.Warn("Failed to cleanup mgmt tap on failed launch", "tap", instance.MgmtTap, "err", err)
		}
		if d.mgmtIPAllocator != nil {
			d.mgmtIPAllocator.Release(instance.ID)
		}
	}

	// Clean up ENI if we auto-created one (not pre-created)
	// Pre-created ENIs are managed by the caller (e.g. ELBv2 service)
	if instance.ENIId != "" && d.vpcService != nil {
		// Only clean up ENIs we created (description starts with "System interface")
		result, err := d.vpcService.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
			NetworkInterfaceIds: []*string{aws.String(instance.ENIId)},
		}, utils.GlobalAccountID)
		if err == nil && len(result.NetworkInterfaces) > 0 {
			desc := aws.StringValue(result.NetworkInterfaces[0].Description)
			if len(desc) >= 16 && desc[:16] == "System interface" {
				if _, delErr := d.vpcService.DeleteNetworkInterface(&ec2.DeleteNetworkInterfaceInput{
					NetworkInterfaceId: aws.String(instance.ENIId),
				}, utils.GlobalAccountID); delErr != nil {
					slog.Warn("Failed to cleanup system ENI", "eniId", instance.ENIId, "err", delErr)
				}
			}
		}
	}
}

// WaitForSystemInstance polls until the instance reaches running state or times out.
func (d *Daemon) WaitForSystemInstance(instanceID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d.Instances.Mu.Lock()
		inst, exists := d.Instances.VMS[instanceID]
		var status vm.InstanceState
		if exists {
			status = inst.Status
		}
		d.Instances.Mu.Unlock()

		if !exists {
			return fmt.Errorf("instance %s disappeared", instanceID)
		}

		switch status {
		case vm.StateRunning:
			return nil
		case vm.StateError, vm.StateShuttingDown, vm.StateTerminated:
			return fmt.Errorf("instance %s in terminal state: %s", instanceID, status)
		}

		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("instance %s did not reach running state within %s", instanceID, timeout)
}
