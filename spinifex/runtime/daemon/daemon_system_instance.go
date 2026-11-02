package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	"github.com/mulgadc/spinifex/spinifex/domains/network/projection"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
)

// Compile-time check that Daemon implements SystemInstanceLauncher.
var _ elbv2domain.SystemInstanceLauncher = (*Daemon)(nil)

// resolveENIAccount returns owner when set, else fallback. A cross-account ENI
// (e.g. an EKS cluster NLB fronting a customer-VPC ENI) carries its own account;
// same-account ENIs leave it empty and inherit the primary/system account.
func resolveENIAccount(owner, fallback string) string {
	if owner != "" {
		return owner
	}
	return fallback
}

// eniInstanceOwner is the owner to stamp on an ENI record when a system VM
// attaches it. System VMs run in the system account but plug into a customer
// ENI, so without the stamp IMDS resolves the instance under the ENI account
// and serves no credentials. It is empty when both accounts are the same.
func eniInstanceOwner(eniAccountID, instanceOwnerID string) string {
	if instanceOwnerID == eniAccountID {
		return ""
	}
	return instanceOwnerID
}

// attachExtraENI attaches one pre-created extra ENI, stamping the instance
// owner in the attach write, and, when the caller asked for an explicit
// DeleteOnTermination, persists it onto the record. An ENI that has to outlive
// its VM is only safe once that flag is stored, so a failed stamp fails the
// attach rather than leaving a disposable NIC behind.
func (d *Daemon) attachExtraENI(eniAccountID string, extra systeminstance.ExtraENIInput, instanceID string, deviceIndex int64, instanceOwnerID string) error {
	if _, err := d.vpcService.AttachENIWithOwner(eniAccountID, extra.ENIID, instanceID, deviceIndex, eniInstanceOwner(eniAccountID, instanceOwnerID)); err != nil {
		return err
	}
	if extra.DeleteOnTermination == nil {
		return nil
	}
	if err := d.vpcService.UpdateENI(eniAccountID, extra.ENIID, func(r *ec2vpc.ENIRecord) {
		r.DeleteOnTermination = extra.DeleteOnTermination
	}); err != nil {
		return fmt.Errorf("set DeleteOnTermination on ENI %s: %w", extra.ENIID, err)
	}
	return nil
}

// LaunchSystemInstance creates and starts a system-managed VM (ELBv2 LB).
// The VM is owned by the system account (GlobalAccountID), is not visible to
// customer DescribeInstances calls, and always boots via direct kernel boot
// (bundled vmlinuz+initramfs, fw_cfg-delivered config). There is no AMI,
// volume, or cloud-init path.
func (d *Daemon) LaunchSystemInstance(input *elbv2domain.SystemInstanceInput) (*elbv2domain.SystemInstanceOutput, error) {
	if err := input.RefuseNonLoadBalancerKind(); err != nil {
		return nil, err
	}
	if input.BootMode == systeminstance.BootAMI {
		return d.launchAMISystemInstance(input)
	}
	return d.launchDirectSystemInstance(input, nil)
}

// launchDirectSystemInstance boots a direct-boot microVM. A nil guest is the
// load balancer; an SQS guest's volume and config were checked by the caller
// before anything here allocates.
func (d *Daemon) launchDirectSystemInstance(input *elbv2domain.SystemInstanceInput, guest sqsGuest) (*elbv2domain.SystemInstanceOutput, error) {
	managedBy := tags.ManagedByELBv2
	if guest != nil {
		managedBy = tags.ManagedBySQS
	}
	// The SQS guests reach the gateway only over their mgmt NIC.
	mgmtRequired := input.Scheme == elbv2domain.SchemeInternal || (guest != nil && guest.needsMgmtNIC())
	mgmtFor := "internal-scheme ALB"
	if guest != nil && guest.needsMgmtNIC() {
		mgmtFor = guest.label()
	}
	accountID := awsidentifiers.GlobalAccountID
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
					{Key: aws.String(tags.ManagedByKey), Value: aws.String(managedBy)},
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
	instance.ManagedBy = managedBy
	ec2Instance.Tags = []*ec2.Tag{
		{Key: aws.String(tags.ManagedByKey), Value: aws.String(managedBy)},
	}
	instance.Reservation = &ec2.Reservation{}
	instance.Reservation.SetReservationId(awsidentifiers.GenerateResourceID("r"))
	instance.Reservation.SetOwnerId(accountID)
	instance.Reservation.Instances = []*ec2.Instance{ec2Instance}
	// Mirror the customer-instance path (domains/ec2/instance/service_impl.go:334)
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
			if eniOut, descErr := d.vpcService.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{
				NetworkInterfaceIds: []*string{aws.String(input.ENIID)},
			}, eniAccountID); descErr == nil && len(eniOut.NetworkInterfaces) > 0 && eniOut.NetworkInterfaces[0].VpcId != nil {
				ec2Instance.SetVpcId(*eniOut.NetworkInterfaces[0].VpcId)
			}
		}
		// Mark ENI as attached to this instance
		if d.vpcService != nil {
			if _, attachErr := d.vpcService.AttachENIWithOwner(eniAccountID, instance.ENIId, instance.ID, 0, eniInstanceOwner(eniAccountID, accountID)); attachErr != nil {
				slog.Error("LaunchSystemInstance: failed to attach ENI", "eniId", instance.ENIId, "instanceId", instance.ID, "instanceOwner", accountID, "err", attachErr)
				d.cleanupFailedSystemInstance(instance, instanceType)
				return nil, fmt.Errorf("attach ENI %s: %w", instance.ENIId, attachErr)
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
				if attachErr := d.attachExtraENI(extraAccount, extra, instance.ID, int64(idx+1), accountID); attachErr != nil {
					slog.Error("LaunchSystemInstance: failed to attach extra ENI", "eniId", extra.ENIID, "instanceId", instance.ID, "err", attachErr)
					d.cleanupFailedSystemInstance(instance, instanceType)
					return nil, fmt.Errorf("attach extra ENI %s: %w", extra.ENIID, attachErr)
				}
			}
			instance.ExtraENIs = append(instance.ExtraENIs, vm.ExtraENI{
				ENIID:         extra.ENIID,
				ENIMac:        extra.ENIMac,
				ENIIP:         extra.ENIIP,
				SubnetID:      extra.SubnetID,
				ENICIDRPrefix: extra.ENICIDRPrefix,
				Gateway:       extra.Gateway,
			})
		}
	} else if input.SubnetID != "" && d.vpcService != nil {
		// Auto-create ENI in subnet
		eniOut, eniErr := d.vpcService.CreateNetworkInterface(context.Background(), &ec2.CreateNetworkInterfaceInput{
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
	if input.Scheme == elbv2domain.SchemeInternetFacing && d.vpcService != nil && instance.ENIId != "" {
		if d.eipService != nil {
			allocOut, allocErr := d.eipService.AllocateAddress(context.Background(), &ec2.AllocateAddressInput{}, eniAccountID)
			if allocErr != nil {
				slog.Error("LaunchSystemInstance: EIP AllocateAddress failed", "instanceId", instance.ID, "err", allocErr)
				d.cleanupFailedSystemInstance(instance, instanceType)
				return nil, fmt.Errorf("allocate public IP for internet-facing ALB: %w", allocErr)
			}
			publicIP = aws.StringValue(allocOut.PublicIp)
			poolName := aws.StringValue(allocOut.PublicIpv4Pool)
			allocID := aws.StringValue(allocOut.AllocationId)

			assocOut, assocErr := d.eipService.AssociateAddress(context.Background(), &ec2.AssociateAddressInput{
				AllocationId:       allocOut.AllocationId,
				NetworkInterfaceId: aws.String(instance.ENIId),
			}, eniAccountID)
			if assocErr != nil {
				slog.Error("LaunchSystemInstance: EIP AssociateAddress failed", "instanceId", instance.ID, "allocationId", allocID, "err", assocErr)
				if _, relErr := d.eipService.ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{
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
			allocatedIP, poolName, allocErr := d.externalIPAM.AllocateIP(context.Background(), region, az, ec2vpc.PurposeENIPublic, "", instance.ENIId, instance.ID)
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
			result, descErr := d.vpcService.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{
				NetworkInterfaceIds: []*string{aws.String(instance.ENIId)},
			}, eniAccountID)
			if descErr == nil && len(result.NetworkInterfaces) > 0 && result.NetworkInterfaces[0].VpcId != nil {
				vpcID = *result.NetworkInterfaces[0].VpcId
			}
			portName := topology.Port(instance.ENIId)
			if natErr := projection.New(d.natsConn).AddNAT(vpcID, publicIP, privateIP, portName, instance.ENIMac); natErr != nil {
				slog.Error("LaunchSystemInstance: vpc.add-nat failed for ALB public IP — rolling back to avoid surfacing an unreachable address",
					"instanceId", instance.ID, "publicIp", publicIP, "pool", poolName, "err", natErr)
				// Timeout may have committed the rule after our window; neutralise before releasing the IP.
				projection.New(d.natsConn).RemoveNAT(vpcID, publicIP, privateIP, portName, instance.ENIMac)
				if clearErr := d.vpcService.UpdateENIPublicIP(eniAccountID, instance.ENIId, "", ""); clearErr != nil {
					slog.Warn("LaunchSystemInstance: failed to clear ENI public IP during NAT-failure rollback",
						"eniId", instance.ENIId, "publicIp", publicIP, "err", clearErr)
				}
				if relErr := d.externalIPAM.ReleaseIP(context.Background(), poolName, publicIP, instance.ENIId); relErr != nil {
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
	sub, subErr := d.natsConn.Subscribe(ec2v1.InstanceCommandSubject(instance.ID), d.handleEC2Events)
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
		mgmtBridge := d.mgmtBridgeName()

		mgmtIP, allocErr := d.mgmtIPAllocator.Allocate(instance.ID)
		if allocErr != nil {
			if mgmtRequired {
				d.cleanupFailedSystemInstance(instance, instanceType)
				return nil, fmt.Errorf("allocate mgmt IP for %s: %w", mgmtFor, allocErr)
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
				if mgmtRequired {
					d.cleanupFailedSystemInstance(instance, instanceType)
					return nil, fmt.Errorf("setup mgmt tap for %s: %w", mgmtFor, tapErr)
				}
				slog.Error("LaunchSystemInstance: failed to setup mgmt tap", "instanceId", instance.ID, "err", tapErr)
			} else {
				d.primeMgmtNeighEntry(instance.ID, mgmtIP, instance.MgmtMAC)
				slog.Info("LaunchSystemInstance: mgmt NIC configured",
					"instanceId", instance.ID, "mgmtIP", mgmtIP, "mgmtMAC", instance.MgmtMAC, "mgmtTap", tapName)
			}
		}
	}

	if guest != nil && guest.needsMgmtNIC() && instance.MgmtIP == "" {
		d.cleanupFailedSystemInstance(instance, instanceType)
		return nil, fmt.Errorf("%s: no mgmt NIC on this node", guest.label())
	}

	// The requested guest ports. The host ports they land on are observed by
	// the launch and reported back through HostfwdPortMap.
	instance.HostfwdPorts = slices.Clone(input.HostfwdPorts)

	// Direct-boot (microvm) path: build the vm.Config directly with microvm
	// machine settings and fw_cfg blobs for network, lb-agent env, and CA cert.
	cfg, err := d.buildDirectBootConfig(instance.ID, input, guest)
	if err != nil {
		d.cleanupFailedSystemInstance(instance, instanceType)
		return nil, fmt.Errorf("build direct-boot config: %w", err)
	}
	instance.Config = cfg
	instance.DirectBoot = true
	if guest != nil {
		netcfg, err := netcfgForInput(input)
		if err != nil {
			d.cleanupFailedSystemInstance(instance, instanceType)
			return nil, fmt.Errorf("render sqs guest netcfg: %w", err)
		}
		guest.record(instance, d.node, netcfg)
		// Mounted and marked in-use by the normal launch path, and only
		// detached when the instance terminates.
		instance.EBSRequests.Mu.Lock()
		instance.EBSRequests.Requests = append(instance.EBSRequests.Requests, vm.EBSRequest{
			Name:                guest.dataVolume(),
			DeviceName:          sqsDataDevice,
			DeleteOnTermination: false,
		})
		instance.EBSRequests.Mu.Unlock()
	}

	// Launch QEMU VM
	t1 := time.Now()
	if err := d.vmMgr.Run(context.Background(), instance); err != nil {
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

	return &elbv2domain.SystemInstanceOutput{
		InstanceID: instance.ID,
		PrivateIP:  privateIP,
		PublicIP:   publicIP,
		HostfwdMap: instance.HostfwdPortMap,
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
//
// Sharing the Prepare/Launch pair with RunInstances means a GPU instance type
// gets the same admission, GPU claim and teardown-time release as a customer
// VM: PrepareRunInstances gates on a free GPU slot, LaunchRunInstances claims
// the device, and vm.Manager's cleanup chain releases it.
func (d *Daemon) launchAMISystemInstance(input *systeminstance.SystemInstanceInput) (*systeminstance.SystemInstanceOutput, error) {
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

	_, instances, instanceType, err := d.instanceService.PrepareRunInstances(context.Background(), runInput, input.AccountID, "")
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
		d.vmMgr.MarkFailed(context.Background(), inst, "mgmt_nic_setup_failed")
		return nil, err
	}

	// Pre-created extra ENIs (e.g. an EKS CP server's flannel-overlay ENI in the
	// worker VPC) must be attached and recorded on the VM before LaunchRunInstances
	// so the launch builds their taps/virtio NICs and cloud-init renders a DHCP
	// stanza per extra MAC. A cross-account ENI is keyed by its own account.
	for idx, extra := range input.ExtraENIs {
		if d.vpcService != nil {
			extraAccount := resolveENIAccount(extra.AccountID, input.AccountID)
			if attachErr := d.attachExtraENI(extraAccount, extra, inst.ID, int64(idx+1), ""); attachErr != nil {
				slog.Error("launchAMISystemInstance: failed to attach extra ENI", "eniId", extra.ENIID, "instanceId", inst.ID, "err", attachErr)
				d.vmMgr.MarkFailed(context.Background(), inst, "extra_eni_attach_failed")
				return nil, fmt.Errorf("attach extra ENI %s: %w", extra.ENIID, attachErr)
			}
		}
		inst.ExtraENIs = append(inst.ExtraENIs, vm.ExtraENI{
			ENIID:         extra.ENIID,
			ENIMac:        extra.ENIMac,
			ENIIP:         extra.ENIIP,
			SubnetID:      extra.SubnetID,
			ENICIDRPrefix: extra.ENICIDRPrefix,
			Gateway:       extra.Gateway,
		})
	}

	d.instanceService.LaunchRunInstances(context.Background(), instances, runInput, instanceType)

	if err := d.verifySystemInstanceLaunched(inst); err != nil {
		return nil, err
	}

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
	return &systeminstance.SystemInstanceOutput{
		InstanceID: inst.ID,
		PrivateIP:  privateIP,
		MgmtIP:     inst.MgmtIP,
	}, nil
}

// verifySystemInstanceLaunched checks that inst actually reached running state
// after LaunchRunInstances. That call is a best-effort batch launcher: volume
// preparation, GPU claim and vmMgr.Run failures each mark the instance failed
// and return nothing, so the outcome is only visible in the VM's state. Without
// this check a system-instance caller (EKS control plane, ELB, Bedrock serving
// VM) receives an instance ID for a VM already being torn down and waits on an
// endpoint that will never answer. Cleanup is left to the MarkFailed chain
// LaunchRunInstances already ran.
func (d *Daemon) verifySystemInstanceLaunched(inst *vm.VM) error {
	if status := d.vmMgr.Status(inst); status != vm.StateRunning {
		return fmt.Errorf("sysinstance: instance %s did not reach running state (status %q)", inst.ID, status)
	}
	return nil
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
	mgmtBridge := d.mgmtBridgeName()
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
	d.primeMgmtNeighEntry(inst.ID, mgmtIP, inst.MgmtMAC)
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
	releaser, ok := d.eipService.(interface{ ReleaseAddressByInstanceID(string) error })
	if !ok {
		return
	}
	if err := releaser.ReleaseAddressByInstanceID(instanceID); err != nil {
		slog.Warn("reclaimSystemInstanceEIP: backstop release failed", "instanceId", instanceID, "err", err)
	}
}

// terminateSystemInstanceLocal stops a VM owned by this node.
func (d *Daemon) terminateSystemInstanceLocal(instanceID string) error {
	instance, exists := d.vmMgr.Get(instanceID)
	if !exists {
		return fmt.Errorf("%w: %s", systeminstance.ErrSystemInstanceNotFound, instanceID)
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

// prepareInstanceRelaunch makes this node's world match an instance record it is
// about to launch from.
//
// Both callers start from a record rather than a request, and a record names
// things by reference: paths to files, an address held in a cluster-wide
// reservation. On a same-node restart the references still resolve; on a recovery
// onto a survivor they point at another node. So both concerns below are the same
// question asked twice — what does this record assume that is not true here yet.
func (d *Daemon) prepareInstanceRelaunch(inst *vm.VM) error {
	// Before the launch, and not fatal. The instance is running on this node
	// either way, and the reservation naming the node it left is what stops that
	// node's own teardown from handing the address to somebody else.
	if d.mgmtIPAllocator != nil {
		d.mgmtIPAllocator.Claim(inst.ID, inst.MgmtIP)
	}
	return d.refreshSystemInstanceState(inst)
}

// refreshSystemInstanceState regenerates the tmpfs-backed fw_cfg blobs that
// QEMU loads at boot. The blobs live under hostprocess.RuntimeDir() (tmpfs on
// production hosts) and are wiped on host reboot while the persisted
// vm.Config still references the same paths. Customer VMs use only paths
// under /var/lib/spinifex/ and are a no-op.
func (d *Daemon) refreshSystemInstanceState(inst *vm.VM) error {
	if inst.ManagedBy == tags.ManagedBySQS {
		if inst.SQSWorkerGuest != nil {
			return d.refreshSQSWorkerState(inst)
		}
		return refuseSQSStoreRelaunch(inst)
	}
	if inst.ManagedBy != tags.ManagedByELBv2 {
		return nil
	}
	if d.elbv2Service == nil {
		return fmt.Errorf("elbv2 service unavailable: cannot refresh fw_cfg blobs for system VM %s", inst.ID)
	}
	ctx := elbv2domain.RecoveryContext{
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
		if _, err := d.eipService.DisassociateAddress(context.Background(), &ec2.DisassociateAddressInput{
			AssociationId: aws.String(instance.PublicIPAssocID),
		}, eniAccount); err != nil {
			slog.Warn("releaseSystemInstanceEIP: DisassociateAddress failed", "instanceId", instance.ID, "associationId", instance.PublicIPAssocID, "err", err)
		}
	}
	if _, err := d.eipService.ReleaseAddress(context.Background(), &ec2.ReleaseAddressInput{
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
	d.vmMgr.MarkFailed(context.Background(), instance, "system_instance_launch_failed")
}

// waitPollSleep is the poll delay seam used by WaitForSystemInstance; tests override it.
var waitPollSleep = time.Sleep

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

		waitPollSleep(500 * time.Millisecond)
	}
	return fmt.Errorf("instance %s did not reach running state within %s", instanceID, timeout)
}

// microvmDir holds the microvm kernel, initramfs and firmware. A variable so
// tests can supply their own.
var microvmDir = vm.MicroVMDir

// buildDirectBootConfig constructs a vm.Config for a direct-boot microVM
// launch. It does not call buildBaseVMConfig — the microvm machine type
// requires no PCIe root ports, no UEFI, and no drives; it boots qboot. Runtime
// paths (PID file, console log, serial socket) are filled in later by startQEMU.
//
// Network topology from input.NICs is serialised to three fw_cfg tmpfiles
// (netcfg, lb-agent-env, ca-cert) under hostprocess.RuntimeDir().
func (d *Daemon) buildDirectBootConfig(instanceID string, input *elbv2domain.SystemInstanceInput, guest sqsGuest) (vm.Config, error) {
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

	firmware := filepath.Join(microvmDir, vm.MicroVMFirmware)
	if _, err := os.Stat(firmware); err != nil {
		return vm.Config{}, fmt.Errorf("microvm firmware %s is missing, so load balancer instances cannot boot; reinstall Spinifex to restore it: %w", firmware, err)
	}

	fwCfg, err := d.writeFwCfgBlobs(instanceID, input)
	if err != nil {
		return vm.Config{}, fmt.Errorf("write fw_cfg blobs: %w", err)
	}
	imageDir := microvmDir
	if guest != nil {
		guestCfg, err := guest.writeFwCfg(instanceID)
		if err != nil {
			return vm.Config{}, fmt.Errorf("write sqs guest fw_cfg blobs: %w", err)
		}
		fwCfg = append(sqsNetcfgOnly(fwCfg), guestCfg...)
		imageDir = guest.imageDir()
	}

	// The LB opts out of mitigations: single-tenant, no untrusted code, and it
	// saves ~200-500ms of boot. SQS guests hold tenant data and keep them on.
	// loglevel 7 lets LB kernel warnings reach the console; SQS logs errors only.
	spec := vm.MicroVMSpec{
		Name:           instanceID,
		Architecture:   architecture,
		VCPUs:          vcpus,
		MemoryMiB:      memMiB,
		Firmware:       firmware,
		ImageDir:       imageDir,
		LogLevel:       3,
		MitigationsOff: guest == nil,
		FwCfg:          fwCfg,
	}
	if guest == nil {
		spec.LogLevel = 7
	}

	// Wire netdevs and devices for each NIC in declaration order.
	// Tap device creation happens later in startQEMU (host-side only here).
	allNICs := buildNICNetdevs(instanceID, input, vm.MicroVMMachineType(), d.guestOverlayMTU())
	spec.NetDevs = allNICs.netdevs
	spec.Devices = allNICs.devices

	cfg := vm.NewMicroVMConfig(spec)
	return cfg, nil
}

// nicNetdevResult holds the QEMU netdev and device entries for all NICs.
type nicNetdevResult struct {
	netdevs []vm.NetDev
	devices []vm.Device
}

// buildNICNetdevs produces QEMU -netdev and -device entries for each NIC in
// input. The NIC order determines the netdev IDs: net0 = primary ENI,
// net1 = mgmt (if present), net2+ = extra ENIs. Extra ENIs beyond the primary
// are only included when corresponding ExtraENIs entries exist. A gap at
// net1 (single-node hosts with no br-mgmt) is expected and fine for QEMU.
func buildNICNetdevs(instanceID string, input *elbv2domain.SystemInstanceInput, machineType string, guestMTU int) nicNetdevResult {
	var res nicNetdevResult

	for i, nic := range input.NICs {
		if !nicProvisioned(i, nic) {
			continue
		}
		netID := fmt.Sprintf("net%d", i)
		// Resolve the tap name from the corresponding ENI.
		tapName := tapNameForNIC(i, nic, instanceID, input)
		// Single-queue, matching the taps the direct-boot path pre-creates.
		// vhost still applies: it is the datapath, not the queue count.
		res.netdevs = append(res.netdevs, vm.TapNetDev(netID, tapName, 1))
		res.devices = append(res.devices, vm.NetDevice(machineType, netID, nic.MAC, 1, guestMTU))
	}

	return res
}

// nicProvisioned reports whether NIC[i] has a real tap/device behind it.
// The mgmt NIC (index 1) is only wired when br-mgmt allocated it a MAC at
// launch (LaunchSystemInstance); single-node hosts without br-mgmt leave it
// blank, so it must be excluded from netdevs, mmio slots, and guest config —
// not just any empty-MAC NIC, only this specific, expected slot.
func nicProvisioned(i int, nic elbv2domain.NICConfig) bool {
	return i != 1 || nic.MAC != ""
}

// tapNameForNIC returns the Linux tap device name for a NIC at the given index.
// Index 0 → primary ENI tap, index 1 → mgmt tap, index 2+ → extra ENI taps.
func tapNameForNIC(idx int, _ elbv2domain.NICConfig, instanceID string, input *elbv2domain.SystemInstanceInput) string {
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
// per-VM tmpfiles under hostprocess.RuntimeDir(). Returns the fw_cfg entries for the
// three blobs and an error if any write fails or the NIC default invariant is
// violated.
func (d *Daemon) writeFwCfgBlobs(instanceID string, input *elbv2domain.SystemInstanceInput) ([]vm.FwCfgEntry, error) {
	runtimeDir := hostprocess.RuntimeDir()

	netcfgPath := netcfgFwCfgPath(instanceID)
	lbenvPath := filepath.Join(runtimeDir, fmt.Sprintf("fwcfg-%s-lbenv.tmp", instanceID))
	cacertPath := filepath.Join(runtimeDir, fmt.Sprintf("fwcfg-%s-cacert.tmp", instanceID))

	netcfg, err := netcfgForInput(input)
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

func netcfgFwCfgPath(instanceID string) string {
	return filepath.Join(hostprocess.RuntimeDir(), fmt.Sprintf("fwcfg-%s-netcfg.tmp", instanceID))
}

// netcfgForInput renders the guest network config. Any unprovisioned mgmt NIC
// is excluded so the guest is only told about interfaces that exist, as in
// buildNICNetdevs/buildDirectBootConfig.
func netcfgForInput(input *elbv2domain.SystemInstanceInput) (string, error) {
	provisionedNICs := make([]elbv2domain.NICConfig, 0, len(input.NICs))
	for i, nic := range input.NICs {
		if nicProvisioned(i, nic) {
			provisionedNICs = append(provisionedNICs, nic)
		}
	}
	return buildNetcfgBlob(provisionedNICs)
}

// buildNetcfgBlob serialises a slice of NICConfig entries to the shell KEY=value
// format consumed by the initramfs init script. Exactly one NIC must have
// IsDefault=true; returns an error otherwise.
func buildNetcfgBlob(nics []elbv2domain.NICConfig) (string, error) {
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
