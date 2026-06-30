package daemon

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// Compile-time check that Daemon implements SystemInstanceLauncher.
var _ handlers_elbv2.SystemInstanceLauncher = (*Daemon)(nil)

// resolveENIAccount returns owner when set, else fallback. A cross-account ENI
// (e.g. an EKS cluster NLB fronting a customer-VPC ENI) carries its own account;
// same-account ENIs leave it empty and inherit the primary/system account.
func resolveENIAccount(owner, fallback string) string {
	if owner != "" {
		return owner
	}
	return fallback
}

// LaunchSystemInstance creates and starts a system-managed VM (ELBv2 LB).
// The VM is owned by the system account (GlobalAccountID), is not visible to
// customer DescribeInstances calls, and always boots via direct kernel boot
// (bundled vmlinuz+initramfs, fw_cfg-delivered config). There is no AMI,
// volume, or cloud-init path.
func (d *Daemon) LaunchSystemInstance(input *handlers_elbv2.SystemInstanceInput) (*handlers_elbv2.SystemInstanceOutput, error) {
	if input.BootMode == sysinstance.BootAMI {
		return d.launchAMISystemInstance(input)
	}
	accountID := utils.GlobalAccountID
	// ENI account may differ from system account — the ENI is created under
	// the caller's account, so lookups/updates must use that account ID.
	eniAccountID := resolveENIAccount(input.AccountID, accountID)

	// Validate instance type
	instanceType, exists := d.resourceMgr.instanceTypes[input.InstanceType]
	if !exists {
		return nil, fmt.Errorf("unknown instance type: %s", input.InstanceType)
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
	if input.IamInstanceProfileArn != "" {
		runInput.IamInstanceProfile = &ec2.IamInstanceProfileSpecification{Arn: aws.String(input.IamInstanceProfileArn)}
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
	// Mirror the customer-instance path (handlers/ec2/instance/service_impl.go:334)
	// so consumers reading instance.Instance (e.g. onInstanceUpHook's NAT
	// republish, device_map, volumes) see the same metadata for system VMs.
	instance.Instance = ec2Instance

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
		// The auto-create-ENI branch below populates VpcId from the freshly
		// created ENI; do the same for pre-created ENIs so consumers reading
		// instance.Instance.VpcId (notably onInstanceUpHook's NAT republish)
		// work uniformly across both paths.
		if d.vpcService != nil {
			if eniOut, descErr := d.vpcService.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
				NetworkInterfaceIds: []*string{aws.String(input.ENIID)},
			}, eniAccountID); descErr == nil && len(eniOut.NetworkInterfaces) > 0 && eniOut.NetworkInterfaces[0].VpcId != nil {
				ec2Instance.SetVpcId(*eniOut.NetworkInterfaces[0].VpcId)
			}
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
				// A cross-account extra ENI (e.g. an EKS cluster NLB fronting a
				// customer-VPC ENI) lives in its own account; the ENI record is
				// account-keyed, so attach under extra.AccountID when set.
				extraAccount := resolveENIAccount(extra.AccountID, eniAccountID)
				if _, attachErr := d.vpcService.AttachENI(extraAccount, extra.ENIID, instance.ID, int64(idx+1)); attachErr != nil {
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
			allocatedIP, poolName, allocErr := d.externalIPAM.AllocateIP(region, az, handlers_ec2_vpc.PurposeENIPublic, "", instance.ENIId, instance.ID)
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
			portName := topology.Port(instance.ENIId)
			if natErr := utils.AddNAT(d.natsConn, vpcID, publicIP, privateIP, portName, instance.ENIMac); natErr != nil {
				slog.Error("LaunchSystemInstance: vpc.add-nat failed for ALB public IP — rolling back to avoid surfacing an unreachable address",
					"instanceId", instance.ID, "publicIp", publicIP, "pool", poolName, "err", natErr)
				// Timeout may have committed the rule after our window; neutralise before releasing the IP.
				utils.PublishNATEvent(d.natsConn, "vpc.delete-nat", vpcID, publicIP, privateIP, portName, instance.ENIMac)
				if clearErr := d.vpcService.UpdateENIPublicIP(eniAccountID, instance.ENIId, "", ""); clearErr != nil {
					slog.Warn("LaunchSystemInstance: failed to clear ENI public IP during NAT-failure rollback",
						"eniId", instance.ENIId, "publicIp", publicIP, "err", clearErr)
				}
				if relErr := d.externalIPAM.ReleaseIP(poolName, publicIP, instance.ENIId); relErr != nil {
					slog.Warn("LaunchSystemInstance: failed to release public IP during NAT-failure rollback",
						"publicIp", publicIP, "pool", poolName, "err", relErr)
				}
				d.cleanupFailedSystemInstance(instance, instanceType)
				return nil, fmt.Errorf("commit NAT rule for internet-facing ALB public IP %s: %w", publicIP, natErr)
			}
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
			instance.MgmtMAC = vm.GenerateMgmtMAC(instance.ID)
			instance.MgmtIP = mgmtIP

			// Inject the allocated MAC and CIDR into NIC[1] so writeFwCfgBlobs
			// carries real values — the ELBv2 service leaves them blank because
			// MAC/IP are only known after daemon allocation.
			if len(input.NICs) > 1 && input.NICs[1].MAC == "" {
				input.NICs[1].MAC = instance.MgmtMAC
				input.NICs[1].CIDR = instance.MgmtIP + "/24"
			}

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

	// Direct-boot (microvm) path: build the vm.Config directly with microvm
	// machine settings and fw_cfg blobs for network, lb-agent env, and CA cert.
	cfg, err := d.buildDirectBootConfig(instance.ID, input)
	if err != nil {
		d.cleanupFailedSystemInstance(instance, instanceType)
		return nil, fmt.Errorf("build direct-boot config: %w", err)
	}
	instance.Config = cfg
	instance.DirectBoot = true

	// Launch QEMU VM
	t1 := time.Now()
	if err := d.vmMgr.Run(instance); err != nil {
		d.cleanupFailedSystemInstance(instance, instanceType)
		return nil, fmt.Errorf("launch instance: %w", err)
	}
	slog.Info("direct-boot timing",
		"instanceId", instance.ID,
		"t1_to_t2_ms", time.Since(t1).Milliseconds())

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

// launchAMISystemInstance is the BootAMI branch of LaunchSystemInstance: a full
// AMI boot (root volume cloned from an AMI, bootstrapped from the Ec2 IMDS
// datasource) for a system-managed VM, with a management-bridge NIC so the
// guest can reach the daemon (NATS/AWSGW) off its tenant VPC subnet.
//
// It mirrors the daemon RunInstances handler's Prepare → Insert → Launch split,
// allocating the mgmt NIC on the VM record between Insert and Launch so the
// fw_cfg netcfg blob built during Launch enumerates both the primary data ENI
// (DHCP) and the static mgmt0 address — a multi-NIC Alpine guest cannot pick the
// right NIC for the Ec2 datasource on its own. The instance is owned by
// input.AccountID (the account its pre-created ENI lives in) and tagged
// input.ManagedBy so customer listings hide it.
func (d *Daemon) launchAMISystemInstance(input *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error) {
	if d.instanceService == nil {
		return nil, errors.New("sysinstance: instance service not initialized")
	}
	if input.ImageID == "" {
		return nil, errors.New("sysinstance: BootAMI requires ImageID")
	}
	if input.AccountID == "" {
		return nil, errors.New("sysinstance: BootAMI requires AccountID")
	}
	if input.ENIID == "" {
		return nil, errors.New("sysinstance: BootAMI requires a pre-created ENI")
	}

	runInput := &ec2.RunInstancesInput{
		ImageId:      aws.String(input.ImageID),
		InstanceType: aws.String(input.InstanceType),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		NetworkInterfaces: []*ec2.InstanceNetworkInterfaceSpecification{{
			NetworkInterfaceId: aws.String(input.ENIID),
			DeviceIndex:        aws.Int64(0),
		}},
	}
	if input.UserData != "" {
		runInput.UserData = aws.String(base64.StdEncoding.EncodeToString([]byte(input.UserData)))
	}
	if input.IamInstanceProfileArn != "" {
		runInput.IamInstanceProfile = &ec2.IamInstanceProfileSpecification{Arn: aws.String(input.IamInstanceProfileArn)}
	}
	if input.ManagedBy != "" {
		runInput.TagSpecifications = []*ec2.TagSpecification{{
			ResourceType: aws.String("instance"),
			Tags:         []*ec2.Tag{{Key: aws.String(tags.ManagedByKey), Value: aws.String(input.ManagedBy)}},
		}}
	}

	_, instances, instanceType, err := d.instanceService.PrepareRunInstances(runInput, input.AccountID, "")
	if err != nil {
		return nil, err
	}
	if len(instances) == 0 {
		return nil, errors.New("sysinstance: PrepareRunInstances returned no instance")
	}
	inst := instances[0]
	inst.ManagedBy = input.ManagedBy

	for _, instance := range instances {
		d.vmMgr.Insert(instance)
	}

	// Management NIC must be set before LaunchRunInstances so the fw_cfg netcfg
	// blob (built during launch) carries the static mgmt0 address.
	if err := d.attachSystemMgmtNIC(inst); err != nil {
		d.vmMgr.MarkFailed(inst, "mgmt_nic_setup_failed")
		return nil, err
	}

	d.instanceService.LaunchRunInstances(instances, runInput, instanceType)

	privateIP := input.ENIIP
	if privateIP == "" && inst.Instance != nil {
		privateIP = aws.StringValue(inst.Instance.PrivateIpAddress)
	}
	slog.Info("LaunchSystemInstance (AMI) completed",
		"instanceId", inst.ID,
		"managedBy", input.ManagedBy,
		"imageId", input.ImageID,
		"mgmtIP", inst.MgmtIP,
		"privateIp", privateIP,
	)
	return &sysinstance.SystemInstanceOutput{
		InstanceID: inst.ID,
		PrivateIP:  privateIP,
		MgmtIP:     inst.MgmtIP,
	}, nil
}

// attachSystemMgmtNIC allocates a management-bridge IP + MAC for a system VM and
// creates the host-side tap on br-mgmt. The mgmt NIC is mandatory for system
// instances that boot from an AMI: their VPC ENI has no route off the tenant
// subnet, so the mgmt NIC is the only path to the daemon (NATS/AWSGW). On
// failure it releases the IP and returns an error so the caller can fail the
// launch.
func (d *Daemon) attachSystemMgmtNIC(inst *vm.VM) error {
	if d.mgmtIPAllocator == nil || d.mgmtBridgeIP == "" {
		return errors.New("sysinstance: management bridge unavailable; system VM would have no route to the daemon")
	}
	mgmtBridge := "br-mgmt"
	if d.config.Daemon.MgmtBridge != "" {
		mgmtBridge = d.config.Daemon.MgmtBridge
	}
	mgmtIP, err := d.mgmtIPAllocator.Allocate(inst.ID)
	if err != nil {
		return fmt.Errorf("allocate mgmt IP: %w", err)
	}
	inst.MgmtMAC = vm.GenerateMgmtMAC(inst.ID)
	inst.MgmtIP = mgmtIP
	tapName := vm.MgmtTapName(inst.ID)
	if err := d.networkPlumber.SetupTap(vm.TapSpec{Name: tapName, Bridge: mgmtBridge}); err != nil {
		d.mgmtIPAllocator.Release(inst.ID)
		inst.MgmtMAC = ""
		inst.MgmtIP = ""
		return fmt.Errorf("setup mgmt tap: %w", err)
	}
	slog.Info("System instance mgmt NIC configured",
		"instanceId", inst.ID, "mgmtIP", mgmtIP, "mgmtMAC", inst.MgmtMAC, "mgmtTap", tapName)
	return nil
}

// TerminateSystemInstance stops and cleans up a system-managed VM. When this
// node does not own the VM, the terminate is routed over NATS to the owning
// daemon: an HA-spread EKS control plane places CP VMs on remote hosts, so a
// cluster-wide teardown invoked on any node must reach the owner to actually
// stop qemu and free the ENI — otherwise the local-only path returns NotFound
// and the teardown deletes a still-attached ENI (InvalidNetworkInterface.InUse).
func (d *Daemon) TerminateSystemInstance(instanceID string) error {
	var termErr error
	if _, exists := d.vmMgr.Get(instanceID); exists {
		termErr = d.terminateSystemInstanceLocal(instanceID)
	} else {
		termErr = d.terminateSystemInstanceRemote(instanceID)
	}

	// Backstop for the no-owner case: terminateSystemInstanceLocal runs the
	// reclaim on the owning node, but when no node owns the VM (already gone) the
	// remote route returns NotFound and never reaches it, so an internet-facing
	// ALB's EIP would orphan. Idempotent — a no-op once the record is released.
	d.reclaimSystemInstanceEIP(instanceID)
	return termErr
}

// reclaimSystemInstanceEIP releases any EIP still recorded against instanceID via
// the authoritative EIP KV, independent of the cached fields on the VM record.
// Idempotent and nil-safe; logs on failure.
func (d *Daemon) reclaimSystemInstanceEIP(instanceID string) {
	if d.eipService == nil {
		return
	}
	if err := d.eipService.ReleaseAddressByInstanceID(instanceID); err != nil {
		slog.Warn("reclaimSystemInstanceEIP: backstop release failed", "instanceId", instanceID, "err", err)
	}
}

// terminateSystemInstanceLocal stops a VM owned by this node.
func (d *Daemon) terminateSystemInstanceLocal(instanceID string) error {
	instance, exists := d.vmMgr.Get(instanceID)
	if !exists {
		return fmt.Errorf("%w: %s", sysinstance.ErrSystemInstanceNotFound, instanceID)
	}

	// Release EIP through the EIP service for system VMs whose public IP was
	// allocated via AllocateAddress (internet-facing ALBs). releaseSystemInstanceEIP
	// uses the VM's cached allocation; reclaimSystemInstanceEIP then reconciles
	// against the EIP KV in case the cache was empty (the authoritative path — this
	// runs on the owning node, including the NATS terminate handler).
	d.releaseSystemInstanceEIP(instance)
	d.reclaimSystemInstanceEIP(instanceID)

	if err := d.vmMgr.Terminate(instanceID); err != nil {
		slog.Error("TerminateSystemInstance: vmMgr.Terminate failed", "instanceId", instanceID, "err", err)
		return fmt.Errorf("terminate: %w", err)
	}

	slog.Info("TerminateSystemInstance completed", "instanceId", instanceID)
	return nil
}

// refreshSystemInstanceState regenerates the tmpfs-backed fw_cfg blobs that
// QEMU loads at boot. The blobs live under utils.RuntimeDir() (tmpfs on
// production hosts) and are wiped on host reboot while the persisted
// vm.Config still references the same paths. Customer VMs use only paths
// under /var/lib/spinifex/ and are a no-op.
func (d *Daemon) refreshSystemInstanceState(inst *vm.VM) error {
	if inst.ManagedBy != tags.ManagedByELBv2 {
		return nil
	}
	if d.elbv2Service == nil {
		return fmt.Errorf("elbv2 service unavailable: cannot refresh fw_cfg blobs for system VM %s", inst.ID)
	}
	ctx := handlers_elbv2.RecoveryContext{
		InstanceID:   inst.ID,
		InstanceType: inst.InstanceType,
		ENIMac:       inst.ENIMac,
		MgmtMAC:      inst.MgmtMAC,
		MgmtIP:       inst.MgmtIP,
	}
	input, err := d.elbv2Service.RebuildSystemInstanceInput(ctx)
	if err != nil {
		return fmt.Errorf("rebuild system instance input: %w", err)
	}
	if _, err := d.writeFwCfgBlobs(inst.ID, input); err != nil {
		return fmt.Errorf("rewrite fw_cfg blobs: %w", err)
	}
	slog.Info("Refreshed system instance state",
		"instanceId", inst.ID, "managedBy", inst.ManagedBy)
	return nil
}

// releaseSystemInstanceEIP disassociates and releases an eipService-allocated
// EIP (internet-facing system instances) and clears the instance's EIP fields so
// a later externalIPAM release path (vm.Manager.Terminate/MarkFailed
// ReleasePublicIP) does not double-release the same IP. No-op when the instance
// holds no eipService allocation. Best-effort: each step is logged, none fatal.
func (d *Daemon) releaseSystemInstanceEIP(instance *vm.VM) {
	if instance == nil || instance.PublicIPAllocID == "" || d.eipService == nil {
		return
	}
	eniAccount := instance.AccountID
	if instance.PublicIPAssocID != "" {
		if _, err := d.eipService.DisassociateAddress(&ec2.DisassociateAddressInput{
			AssociationId: aws.String(instance.PublicIPAssocID),
		}, eniAccount); err != nil {
			slog.Warn("releaseSystemInstanceEIP: DisassociateAddress failed", "instanceId", instance.ID, "associationId", instance.PublicIPAssocID, "err", err)
		}
	}
	if _, err := d.eipService.ReleaseAddress(&ec2.ReleaseAddressInput{
		AllocationId: aws.String(instance.PublicIPAllocID),
	}, eniAccount); err != nil {
		slog.Warn("releaseSystemInstanceEIP: ReleaseAddress failed", "instanceId", instance.ID, "allocationId", instance.PublicIPAllocID, "err", err)
	} else {
		slog.Info("releaseSystemInstanceEIP: released EIP", "instanceId", instance.ID, "ip", instance.PublicIP, "allocationId", instance.PublicIPAllocID)
	}
	instance.PublicIP = ""
	instance.PublicIPPool = ""
	instance.PublicIPAllocID = ""
	instance.PublicIPAssocID = ""
	d.vmMgr.UpdateState(instance.ID, func(v *vm.VM) {
		v.PublicIP = ""
		v.PublicIPPool = ""
		v.PublicIPAllocID = ""
		v.PublicIPAssocID = ""
	})
}

// cleanupFailedSystemInstance handles cleanup when a system instance launch
// fails after partial setup (state added, volumes created, etc). Releases an
// eipService-allocated EIP first (MarkFailed's ReleasePublicIP only knows the
// externalIPAM path, so an associated EIP would otherwise leak), then delegates
// to vm.Manager.MarkFailed which runs the synchronous teardown chain (volume
// unmount/delete, tap cleanup, ENI delete, IP release, resource deallocation,
// state migration to terminated KV).
func (d *Daemon) cleanupFailedSystemInstance(instance *vm.VM, _ *ec2.InstanceTypeInfo) {
	d.releaseSystemInstanceEIP(instance)
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

// buildDirectBootConfig constructs a vm.Config for a direct-boot microVM
// launch. It does not call buildBaseVMConfig — the microvm machine type
// requires no PCIe root ports, no UEFI/BIOS, and no drives. Runtime paths
// (PID file, console log, serial socket) are filled in later by startQEMU.
//
// Network topology from input.NICs is serialised to three fw_cfg tmpfiles
// (netcfg, lb-agent-env, ca-cert) under utils.RuntimeDir().
func (d *Daemon) buildDirectBootConfig(instanceID string, input *handlers_elbv2.SystemInstanceInput) (vm.Config, error) {
	it := d.resourceMgr.instanceTypes[input.InstanceType]
	architecture := "x86_64"
	if it != nil && it.ProcessorInfo != nil && len(it.ProcessorInfo.SupportedArchitectures) > 0 && it.ProcessorInfo.SupportedArchitectures[0] != nil {
		architecture = *it.ProcessorInfo.SupportedArchitectures[0]
	}
	vcpus := 1
	memMiB := 128
	if it != nil {
		vcpus = int(instanceTypeVCPUs(it))
		memMiB = int(instanceTypeMemoryMiB(it))
	}

	const imagePath = "/usr/share/spinifex/microvm"

	fwCfg, err := d.writeFwCfgBlobs(instanceID, input)
	if err != nil {
		return vm.Config{}, fmt.Errorf("write fw_cfg blobs: %w", err)
	}

	// Build kernel cmdline.
	//
	// QEMU microvm maps virtio-mmio slots at 0xfeb00000 + n*0x200 with IRQ 5+n.
	// Without virtio_mmio.device params the kernel never discovers the devices
	// (auto-kernel-cmdline does not append when -append is specified).
	//
	// Boot-time perf flags (Phase 2):
	//   quiet loglevel=3        — suppress printk to ttyS0 (115200 baud serial
	//                             writes dominate boot time; warnings+errors only)
	//   mitigations=off         — skip spectre/meltdown microcode patching at
	//                             boot. Safe here: KVM guest on trusted host CPU,
	//                             single-tenant LB workload, no untrusted code in
	//                             the VM. Saves ~200-500ms.
	//   tsc=reliable            — skip TSC calibration loop on KVM (host already
	//                             vouched for invariant TSC)
	//   no_timer_check          — skip APIC/PIT cross-check; microvm has no PIT
	//   reboot=t                — triple-fault reboot; skip ACPI/keyboard probes
	//   i8042.no{pnp,aux,kbd}   — microvm has no PS/2; skip i8042 probe timeouts
	var sb strings.Builder
	sb.WriteString("console=ttyS0 quiet loglevel=3 mitigations=off tsc=reliable no_timer_check reboot=t i8042.nopnp i8042.noaux i8042.nokbd")
	for i := range input.NICs {
		fmt.Fprintf(&sb, " virtio_mmio.device=0x200@0x%x:%d",
			0xfeb00000+i*0x200, 5+i)
	}
	cmdline := sb.String()

	machineType := microvmMachineType()

	cfg := vm.Config{
		Name:          instanceID,
		EnableKVM:     true,
		NoGraphic:     true,
		Architecture:  architecture,
		CPUType:       "host,-pmu",
		CPUCount:      vcpus,
		Memory:        memMiB,
		MachineType:   machineType,
		KernelImage:   filepath.Join(imagePath, "vmlinuz"),
		Initrd:        filepath.Join(imagePath, "initramfs.cpio.gz"),
		KernelCmdline: cmdline,
		FwCfg:         fwCfg,
	}

	// Wire netdevs and devices for each NIC in declaration order.
	// Tap device creation happens later in startQEMU (host-side only here).
	allNICs := buildNICNetdevs(instanceID, input, machineType)
	cfg.NetDevs = append(cfg.NetDevs, allNICs.netdevs...)
	cfg.Devices = append(cfg.Devices, allNICs.devices...)

	return cfg, nil
}

// nicNetdevResult holds the QEMU netdev and device entries for all NICs.
type nicNetdevResult struct {
	netdevs []vm.NetDev
	devices []vm.Device
}

// microvmMachineType returns the QEMU -M string for the microvm machine.
// isa-serial is kept on permanently for per-VM console log access — the
// boot-time saving from turning it off is not worth losing console output.
func microvmMachineType() string {
	return "microvm,pic=off,pit=off,rtc=on,acpi=on,isa-serial=on"
}

// buildNICNetdevs produces QEMU -netdev and -device entries for each NIC in
// input. The NIC order determines the netdev IDs: net0 = primary ENI,
// net1 = mgmt (if present), net2+ = extra ENIs. Extra ENIs beyond the primary
// are only included when corresponding ExtraENIs entries exist.
func buildNICNetdevs(instanceID string, input *handlers_elbv2.SystemInstanceInput, machineType string) nicNetdevResult {
	var res nicNetdevResult

	for i, nic := range input.NICs {
		netID := fmt.Sprintf("net%d", i)
		// Resolve the tap name from the corresponding ENI.
		tapName := tapNameForNIC(i, nic, instanceID, input)
		res.netdevs = append(res.netdevs, vm.NetDev{
			Value: fmt.Sprintf("tap,id=%s,ifname=%s,script=no,downscript=no", netID, tapName),
		})
		res.devices = append(res.devices, vm.NetDevice(machineType, netID, nic.MAC))
	}

	return res
}

// tapNameForNIC returns the Linux tap device name for a NIC at the given index.
// Index 0 → primary ENI tap, index 1 → mgmt tap, index 2+ → extra ENI taps.
func tapNameForNIC(idx int, _ handlers_elbv2.NICConfig, instanceID string, input *handlers_elbv2.SystemInstanceInput) string {
	switch idx {
	case 0:
		return vm.TapDeviceName(input.ENIID)
	case 1:
		return vm.MgmtTapName(instanceID)
	default:
		extraIdx := idx - 2
		if extraIdx < len(input.ExtraENIs) {
			return vm.TapDeviceName(input.ExtraENIs[extraIdx].ENIID)
		}
		return fmt.Sprintf("tap-unknown-%d", idx)
	}
}

// writeFwCfgBlobs serialises NIC configuration, lb-agent env, and CA cert to
// per-VM tmpfiles under utils.RuntimeDir(). Returns the fw_cfg entries for the
// three blobs and an error if any write fails or the NIC default invariant is
// violated.
func (d *Daemon) writeFwCfgBlobs(instanceID string, input *handlers_elbv2.SystemInstanceInput) ([]vm.FwCfgEntry, error) {
	runtimeDir := utils.RuntimeDir()

	netcfgPath := filepath.Join(runtimeDir, fmt.Sprintf("fwcfg-%s-netcfg.tmp", instanceID))
	lbenvPath := filepath.Join(runtimeDir, fmt.Sprintf("fwcfg-%s-lbenv.tmp", instanceID))
	cacertPath := filepath.Join(runtimeDir, fmt.Sprintf("fwcfg-%s-cacert.tmp", instanceID))

	netcfg, err := buildNetcfgBlob(input.NICs)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(netcfgPath, []byte(netcfg), 0600); err != nil {
		return nil, fmt.Errorf("write netcfg blob: %w", err)
	}

	if err := os.WriteFile(lbenvPath, []byte(input.LBAgentEnv), 0600); err != nil {
		_ = os.Remove(netcfgPath)
		return nil, fmt.Errorf("write lb-agent-env blob: %w", err)
	}

	if err := os.WriteFile(cacertPath, []byte(input.CACert), 0600); err != nil {
		_ = os.Remove(netcfgPath)
		_ = os.Remove(lbenvPath)
		return nil, fmt.Errorf("write ca-cert blob: %w", err)
	}

	slog.Debug("wrote fw_cfg blobs",
		"instanceId", instanceID,
		"netcfg", netcfgPath,
		"lbenv", lbenvPath,
		"cacert", cacertPath,
	)

	return []vm.FwCfgEntry{
		{Name: "opt/spinifex/netcfg", File: netcfgPath},
		{Name: "opt/spinifex/lb-agent-env", File: lbenvPath},
		{Name: "opt/spinifex/ca-cert", File: cacertPath},
	}, nil
}

// buildNetcfgBlob serialises a slice of NICConfig entries to the shell KEY=value
// format consumed by the initramfs init script. Exactly one NIC must have
// IsDefault=true; returns an error otherwise.
func buildNetcfgBlob(nics []handlers_elbv2.NICConfig) (string, error) {
	defaultCount := 0
	for _, n := range nics {
		if n.IsDefault {
			defaultCount++
		}
	}
	if defaultCount != 1 {
		return "", fmt.Errorf("netcfg: exactly one NIC must have IsDefault=true, got %d", defaultCount)
	}

	var sb strings.Builder
	for i, n := range nics {
		prefix := fmt.Sprintf("NIC%d", i)
		fmt.Fprintf(&sb, "%s_MAC=%s\n", prefix, n.MAC)
		if n.CIDR != "" {
			fmt.Fprintf(&sb, "%s_CIDR=%s\n", prefix, n.CIDR)
		}
		if n.Gateway != "" {
			fmt.Fprintf(&sb, "%s_GW=%s\n", prefix, n.Gateway)
		}
		if n.IsDefault {
			fmt.Fprintf(&sb, "%s_DEFAULT=1\n", prefix)
		} else {
			fmt.Fprintf(&sb, "%s_DEFAULT=0\n", prefix)
		}
		if n.RouteDst != "" {
			fmt.Fprintf(&sb, "%s_ROUTE_DST=%s\n", prefix, n.RouteDst)
		}
		if n.RouteVia != "" {
			fmt.Fprintf(&sb, "%s_ROUTE_VIA=%s\n", prefix, n.RouteVia)
		}
	}
	return sb.String(), nil
}
