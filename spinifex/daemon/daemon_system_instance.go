package daemon

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/mulgadc/spinifex/spinifex/tags"
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

	// Build a RunInstancesInput for the instance service.
	// Tag the instance as ELBv2-managed so the UI hides it from
	// customer-facing listings (Nodes page).
	runInput := &ec2.RunInstancesInput{
		InstanceType: aws.String(input.InstanceType),
		ImageId:      aws.String(input.ImageID),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String("instance"),
				Tags: []*ec2.Tag{
					{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByELBv2)},
				},
			},
		},
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
	instance.ManagedBy = tags.ManagedByELBv2
	ec2Instance.Tags = []*ec2.Tag{
		{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByELBv2)},
	}
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

	// Allocate public IP for internet-facing ALBs. Route through the EIP
	// service when available so an EIPRecord is created in the KV bucket —
	// otherwise AWS SDK callers (OpenTofu) can't observe the EIP via
	// DescribeAddresses and their provisioning flow hangs. Falls back to
	// direct IPAM allocation for daemons where the EIP service isn't wired.
	publicIP := ""
	if input.Scheme == handlers_elbv2.SchemeInternetFacing && d.vpcService != nil && instance.ENIId != "" {
		if d.eipService != nil {
			allocOut, allocErr := d.eipService.AllocateAddress(&ec2.AllocateAddressInput{}, eniAccountID)
			if allocErr != nil {
				slog.Error("LaunchSystemInstance: EIP AllocateAddress failed", "instanceId", instance.ID, "err", allocErr)
				d.cleanupFailedSystemInstance(instance, instanceType)
				return nil, fmt.Errorf("allocate public IP for internet-facing ALB: %w", allocErr)
			}
			publicIP = aws.StringValue(allocOut.PublicIp)
			poolName := aws.StringValue(allocOut.PublicIpv4Pool)
			allocID := aws.StringValue(allocOut.AllocationId)

			assocOut, assocErr := d.eipService.AssociateAddress(&ec2.AssociateAddressInput{
				AllocationId:       allocOut.AllocationId,
				NetworkInterfaceId: aws.String(instance.ENIId),
			}, eniAccountID)
			if assocErr != nil {
				slog.Error("LaunchSystemInstance: EIP AssociateAddress failed", "instanceId", instance.ID, "allocationId", allocID, "err", assocErr)
				if _, relErr := d.eipService.ReleaseAddress(&ec2.ReleaseAddressInput{
					AllocationId: allocOut.AllocationId,
				}, eniAccountID); relErr != nil {
					slog.Warn("LaunchSystemInstance: failed to release EIP after associate failure", "allocationId", allocID, "err", relErr)
				}
				d.cleanupFailedSystemInstance(instance, instanceType)
				return nil, fmt.Errorf("associate public IP for internet-facing ALB: %w", assocErr)
			}

			if updateErr := d.vpcService.UpdateENIPublicIP(eniAccountID, instance.ENIId, publicIP, poolName); updateErr != nil {
				slog.Warn("LaunchSystemInstance: failed to update ENI with public IP", "eniId", instance.ENIId, "err", updateErr)
			}
			instance.PublicIP = publicIP
			instance.PublicIPPool = poolName
			instance.PublicIPAllocID = allocID
			instance.PublicIPAssocID = aws.StringValue(assocOut.AssociationId)
			slog.Info("LaunchSystemInstance: allocated public IP via EIP service",
				"instanceId", instance.ID,
				"publicIp", publicIP,
				"pool", poolName,
				"allocationId", allocID,
				"associationId", instance.PublicIPAssocID,
			)
		} else if d.externalIPAM != nil {
			region := ""
			az := ""
			if d.config != nil {
				region = d.config.Region
				az = d.config.AZ
			}
			allocatedIP, poolName, allocErr := d.externalIPAM.AllocateIP(region, az, "auto_assign", "", instance.ENIId, instance.ID)
			if allocErr != nil {
				slog.Error("LaunchSystemInstance: failed to allocate public IP for internet-facing ALB", "instanceId", instance.ID, "err", allocErr)
				d.cleanupFailedSystemInstance(instance, instanceType)
				return nil, fmt.Errorf("allocate public IP for internet-facing ALB: %w", allocErr)
			}
			publicIP = allocatedIP
			if updateErr := d.vpcService.UpdateENIPublicIP(eniAccountID, instance.ENIId, publicIP, poolName); updateErr != nil {
				slog.Warn("LaunchSystemInstance: failed to update ENI with public IP", "eniId", instance.ENIId, "err", updateErr)
			}
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
			slog.Info("LaunchSystemInstance: allocated public IP via direct IPAM",
				"instanceId", instance.ID,
				"publicIp", publicIP,
				"pool", poolName,
			)
		}
	}

	// Add to daemon state so LaunchInstance can find it
	d.vmMgr.Insert(instance)

	if err := d.WriteState(); err != nil {
		slog.Warn("LaunchSystemInstance: failed to write state", "instanceId", instance.ID, "err", err)
	}

	// Subscribe to per-instance NATS topic for terminate commands.
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
		instance.DevMAC = vm.GenerateDevMAC(instance.ID)
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

			tapName := vm.MgmtTapName(instance.ID)
			tapErr := d.networkPlumber.SetupTap(vm.TapSpec{Name: tapName, Bridge: mgmtBridge})
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
	if err := d.vmMgr.Run(instance); err != nil {
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
	instance, exists := d.vmMgr.Get(instanceID)
	if !exists {
		return fmt.Errorf("instance %s not found", instanceID)
	}

	// Release EIP through the EIP service for system VMs whose public IP was
	// allocated via AllocateAddress (internet-facing ALBs). Clears the fields
	// so vm.Manager.Terminate's ReleasePublicIP doesn't double-release the
	// same IP via externalIPAM.
	if instance.PublicIPAllocID != "" && d.eipService != nil {
		eniAccount := instance.AccountID
		if instance.PublicIPAssocID != "" {
			if _, err := d.eipService.DisassociateAddress(&ec2.DisassociateAddressInput{
				AssociationId: aws.String(instance.PublicIPAssocID),
			}, eniAccount); err != nil {
				slog.Warn("TerminateSystemInstance: DisassociateAddress failed", "instanceId", instanceID, "associationId", instance.PublicIPAssocID, "err", err)
			}
		}
		if _, err := d.eipService.ReleaseAddress(&ec2.ReleaseAddressInput{
			AllocationId: aws.String(instance.PublicIPAllocID),
		}, eniAccount); err != nil {
			slog.Warn("TerminateSystemInstance: ReleaseAddress failed", "instanceId", instanceID, "allocationId", instance.PublicIPAllocID, "err", err)
		} else {
			slog.Info("TerminateSystemInstance: released EIP", "instanceId", instanceID, "ip", instance.PublicIP, "allocationId", instance.PublicIPAllocID)
		}
		d.vmMgr.UpdateState(instance.ID, func(v *vm.VM) {
			v.PublicIP = ""
			v.PublicIPPool = ""
			v.PublicIPAllocID = ""
			v.PublicIPAssocID = ""
		})
	}

	if err := d.vmMgr.Terminate(instanceID); err != nil {
		slog.Error("TerminateSystemInstance: vmMgr.Terminate failed", "instanceId", instanceID, "err", err)
		return fmt.Errorf("terminate: %w", err)
	}

	slog.Info("TerminateSystemInstance completed", "instanceId", instanceID)
	return nil
}

// cleanupFailedSystemInstance handles cleanup when a system instance launch
// fails after partial setup (state added, volumes created, etc). Delegates
// to vm.Manager.MarkFailed which runs the synchronous teardown chain
// (volume unmount/delete, tap cleanup, ENI delete, IP release, resource
// deallocation, state migration to terminated KV).
func (d *Daemon) cleanupFailedSystemInstance(instance *vm.VM, _ *ec2.InstanceTypeInfo) {
	d.vmMgr.MarkFailed(instance, "system_instance_launch_failed")
}

// WaitForSystemInstance polls until the instance reaches running state or times out.
func (d *Daemon) WaitForSystemInstance(instanceID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status vm.InstanceState
		exists := d.vmMgr.UpdateState(instanceID, func(v *vm.VM) { status = v.Status })

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
