package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
)

// A DB instance is a dual-NIC system VM: the primary NIC sits in the shared RDS
// system VPC so the in-guest agent can reach the gateway, and the customer ENI
// is injected cross-account as pure ingress. DB subnets rarely have NAT egress.

const (
	// The guest locates the data volume by serial, not by this name; a fixed
	// name only keeps the attachment record predictable across replaces.
	dataVolumeDevice = "/dev/sdf"

	// Links an ENI or volume back to its DB instance, so a teardown or leak
	// sweep can find them without the KV record.
	rdsInstanceTagKey = "spinifex:rds-db-instance"

	// Stamped on the engine AMIs by their image manifest; an EngineVersion
	// request resolves against these.
	engineTagKey             = "engine"
	engineVersionTagKey      = "engine-version"
	dataVolumeContractTagKey = "rds-data-volume-contract"
	dataVolumeContractV1     = "format-auth-v1"

	attachRequestTimeout = 30 * time.Second

	// A fresh budget rather than the caller's remainder: the unwind is often
	// triggered by that deadline expiring, and a dead context deletes nothing.
	rollbackTimeout = 60 * time.Second
)

// A DB VM must never fall back to another image, so callers surface this as a
// validation failure rather than launching something else.
var ErrEngineAMINotFound = errors.New("rds: no AMI found for engine")

// The catalog registers every engine's system image under this prefix, so the
// error can name the image an operator has to import.
const engineImageNamePrefix = "spinifex-rds-"

// An engine image no deployment ever imported is a gap no retry closes, so the
// failure carries a code: a bare error resolves to ServerInternal, which the
// caller reads as a transient fault and retries.
func engineAMINotFound(engine, version string) error {
	requested := "an engine image for " + engine
	if version != "" {
		requested = "version " + version + " for " + engine
	}
	return fmt.Errorf("%w: %w", ErrEngineAMINotFound, awserrors.Errorf(
		awserrors.ErrorInvalidParameterCombination,
		"Cannot find %s; this deployment has no %s%s system image installed",
		requested, engineImageNamePrefix, engine))
}

type launchVPCProvisioner interface {
	CreateNetworkInterface(ctx context.Context, input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error)
	DeleteNetworkInterface(ctx context.Context, input *ec2.DeleteNetworkInterfaceInput, accountID string) (*ec2.DeleteNetworkInterfaceOutput, error)
	DetachENI(ctx context.Context, accountID, eniID string) error
	// A replace re-attaches the persisted customer ENI, so it has to read back
	// the MAC the launcher needs; only the IP is on the DB instance record.
	DescribeNetworkInterfaces(ctx context.Context, input *ec2.DescribeNetworkInterfacesInput, accountID string) (*ec2.DescribeNetworkInterfacesOutput, error)
	// Re-associates the customer ENI's security groups in place, which is what
	// makes a VpcSecurityGroupIds modify need no VM replace and no new IP.
	ModifyNetworkInterfaceAttribute(ctx context.Context, input *ec2.ModifyNetworkInterfaceAttributeInput, accountID string) (*ec2.ModifyNetworkInterfaceAttributeOutput, error)
	// The system NIC's own group, ensured per launch rather than at daemon
	// start, so a deployment whose system VPC was rebuilt still gets one.
	CreateSecurityGroup(ctx context.Context, input *ec2.CreateSecurityGroupInput, accountID string) (*ec2.CreateSecurityGroupOutput, error)
	DescribeSecurityGroups(ctx context.Context, input *ec2.DescribeSecurityGroupsInput, accountID string) (*ec2.DescribeSecurityGroupsOutput, error)
	// Read to configure the customer ENI statically: it must not depend on the
	// OVN DHCP lease being renewed, so its prefix and gateway are resolved once
	// at launch time instead.
	DescribeSubnets(ctx context.Context, input *ec2.DescribeSubnetsInput, accountID string) (*ec2.DescribeSubnetsOutput, error)
}

// System-managed VMs get a mgmt-bridge NIC alongside their VPC NICs, which is
// how the agent reaches the gateway from a private subnet.
type launchInstanceLauncher interface {
	LaunchSystemInstance(input *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error)
	TerminateSystemInstance(instanceID string) error
}

type launchAMIResolver interface {
	DescribeImages(ctx context.Context, input *ec2.DescribeImagesInput, accountID string) (*ec2.DescribeImagesOutput, error)
}

type launchVolumeProvisioner interface {
	CreateVolume(ctx context.Context, input *ec2.CreateVolumeInput, accountID string) (*ec2.Volume, error)
	DeleteVolume(ctx context.Context, input *ec2.DeleteVolumeInput, accountID string) (*ec2.DeleteVolumeOutput, error)
}

// Split from launchVolumeProvisioner because attach is routed to the node
// owning the VM, not answered by whichever node picks the request up.
type volumeAttacher interface {
	AttachVolume(ctx context.Context, accountID, instanceID, volumeID, device string) (string, error)
}

type LaunchDeps struct {
	Config    *config.Config
	SystemVPC handlers_systemvpc.Deps
	VPC       launchVPCProvisioner
	Instance  launchInstanceLauncher
	Image     launchAMIResolver
	Volume    launchVolumeProvisioner
	Attacher  volumeAttacher
}

// Everything here is already validated by the caller: the launch helper
// resolves and wires, it does not police the AWS API surface.
type LaunchInput struct {
	DBInstanceIdentifier string
	// The customer account owning the DB, subnet and customer ENI. The VM
	// itself runs in the system account.
	AccountID             string
	SubnetID              string
	SecurityGroupIDs      []string
	Engine                string
	EngineVersion         string
	InstanceType          string
	AllocatedStorage      int64
	UserData              string
	IamInstanceProfileArn string

	// A replace re-uses the DB instance's persisted customer ENI and data
	// volume instead of minting new ones: the endpoint address and the
	// datadir are the identity that must survive the VM. Both empty is a
	// create, which builds them.
	ExistingCustomerENI string
	ExistingDataVolume  string
}

type LaunchOutput struct {
	InstanceID string
	// The system ENI is disposable — a replace makes a new one. The customer
	// ENI is the stable endpoint: its IP is the DNS target and survives.
	SystemENIID string
	// The region's RDS system group, re-derived on every launch so a record
	// written before the group existed is corrected by the next replace.
	SystemSGID       string
	CustomerENIID    string
	CustomerENIIP    string
	DataVolumeID     string
	DataVolumeSerial string
	// The volume's own reported state, not an echo of the request, so
	// DescribeDBInstances reports encryption the way EC2 does. Meaningful only
	// when this launch created the volume; a replace or restore attaches one
	// whose encryption the record already carries.
	DataVolumeEncrypted bool
	CreatedDataVolume   bool
	// Tears down everything this launch created, for a caller that fails after
	// it returned. The launch runs it itself on its own failures, so it is only
	// ever invoked once.
	Unwind func(context.Context)
}

// On any failure every resource this call created is torn down, so a retried
// create does not accumulate orphan ENIs, volumes and VMs.
func LaunchDBInstanceVM(ctx context.Context, deps LaunchDeps, in LaunchInput) (out *LaunchOutput, err error) {
	if err := validateLaunchInput(in); err != nil {
		return nil, err
	}
	region, az := deps.Config.Region, deps.Config.AZ

	amiID, err := resolveEngineAMI(ctx, deps.Image, in.Engine, in.EngineVersion)
	if err != nil {
		return nil, err
	}

	sysRefs, err := EnsureSystemVPC(ctx, deps.SystemVPC, &deps.Config.RDS, utils.GlobalAccountID, region)
	if err != nil {
		return nil, err
	}
	systemSubnetID := sysRefs.PrivateSubnetIDs[0]

	systemSGID, err := EnsureSystemSecurityGroup(ctx, deps.VPC, utils.GlobalAccountID, region, sysRefs.VpcID)
	if err != nil {
		return nil, err
	}

	// Unwind in reverse creation order on any failure below. Each step appends
	// its own undo as soon as the resource exists.
	var rollback []func(context.Context)

	// The VM is torn down first regardless of creation order: the ENIs and data
	// volume are attached while it runs, and deleting those is rejected as InUse.
	var terminateVM func(context.Context)

	// A context detached from the caller's, so a create that failed *because*
	// the request deadline expired can still clean up.
	unwind := func(ctx context.Context) {
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		if terminateVM != nil {
			terminateVM(rbCtx)
		}
		for _, undo := range slices.Backward(rollback) {
			undo(rbCtx)
		}
	}

	defer func() {
		if err != nil {
			unwind(ctx)
		}
	}()

	systemENI, err := createLaunchENI(ctx, deps.VPC, utils.GlobalAccountID, systemSubnetID, []string{systemSGID},
		"RDS management NIC for "+in.DBInstanceIdentifier, in.DBInstanceIdentifier, false)
	if err != nil {
		return nil, err
	}
	rollback = append(rollback, func(ctx context.Context) {
		deleteLaunchENI(ctx, deps.VPC, utils.GlobalAccountID, systemENI.id)
	})

	// A replace adopts the ENI the endpoint already resolves to; only a create
	// mints one, and only a create may unwind one — deleting the persisted ENI
	// on a failed replace would take the endpoint with it.
	customerENI, err := resolveCustomerENI(ctx, deps.VPC, in)
	if err != nil {
		return nil, err
	}
	if in.ExistingCustomerENI == "" {
		rollback = append(rollback, func(ctx context.Context) {
			deleteLaunchENI(ctx, deps.VPC, in.AccountID, customerENI.id)
		})
	}

	// The customer ENI is configured statically, not via DHCP: OVN's 3600s
	// lease is never renewed by the guest's one-shot udhcpc, so a DHCP customer
	// ENI goes address-less an hour after boot. Resolve its prefix and gateway
	// once here instead of depending on the lease surviving the VM's lifetime.
	customerPrefix, customerGateway, err := customerENISubnetGateway(ctx, deps.VPC, in.AccountID, in.SubnetID)
	if err != nil {
		return nil, fmt.Errorf("rds: resolve customer subnet network for %s: %w", in.DBInstanceIdentifier, err)
	}

	sysOut, err := deps.Instance.LaunchSystemInstance(&sysinstance.SystemInstanceInput{
		BootMode:     sysinstance.BootAMI,
		ManagedBy:    tags.ManagedByRDS,
		InstanceType: in.InstanceType,
		ImageID:      amiID,
		// The VM and its primary NIC live in the system account; the customer
		// ENI carries its own account so the daemon updates the right record.
		AccountID: utils.GlobalAccountID,
		SubnetID:  systemSubnetID,
		ENIID:     systemENI.id,
		ENIMac:    systemENI.mac,
		ENIIP:     systemENI.ip,
		ExtraENIs: []sysinstance.ExtraENIInput{{
			ENIID:         customerENI.id,
			ENIMac:        customerENI.mac,
			ENIIP:         customerENI.ip,
			SubnetID:      in.SubnetID,
			AccountID:     in.AccountID,
			ENICIDRPrefix: customerPrefix,
			Gateway:       customerGateway,
			// The endpoint is the ENI's address, so it has to survive the
			// terminate half of a replace the way the datadir volume does.
			// A DeleteDBInstance deletes it explicitly once the VM is gone.
			DeleteOnTermination: aws.Bool(false),
		}},
		UserData:              in.UserData,
		IamInstanceProfileArn: in.IamInstanceProfileArn,
	})
	if err != nil {
		return nil, fmt.Errorf("rds: launch DB VM for %s: %w", in.DBInstanceIdentifier, err)
	}
	if sysOut == nil || sysOut.InstanceID == "" {
		return nil, fmt.Errorf("rds: launch DB VM for %s: launcher returned no instance", in.DBInstanceIdentifier)
	}
	instanceID := sysOut.InstanceID
	terminateVM = func(ctx context.Context) {
		if termErr := deps.Instance.TerminateSystemInstance(instanceID); termErr != nil &&
			!errors.Is(termErr, sysinstance.ErrSystemInstanceNotFound) {
			slog.WarnContext(ctx, "rds: rollback terminate of failed DB VM failed",
				"dbInstance", in.DBInstanceIdentifier, "instanceId", instanceID, "err", termErr)
		}
	}

	// A replace re-attaches the datadir it already has; only a create makes one,
	// and only a create may unwind one.
	volumeID, volumeEncrypted := in.ExistingDataVolume, false
	if volumeID == "" {
		// Kept separate from the boot volume so a replace can discard the boot
		// volume and keep the datadir, and owned by the system account.
		volume, volErr := deps.Volume.CreateVolume(ctx, &ec2.CreateVolumeInput{
			AvailabilityZone: aws.String(az),
			Size:             aws.Int64(in.AllocatedStorage),
			VolumeType:       aws.String("gp3"),
			TagSpecifications: []*ec2.TagSpecification{{
				ResourceType: aws.String("volume"),
				Tags: []*ec2.Tag{
					{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByRDS)},
					{Key: aws.String(rdsInstanceTagKey), Value: aws.String(in.DBInstanceIdentifier)},
				},
			}},
		}, utils.GlobalAccountID)
		if volErr != nil {
			return nil, fmt.Errorf("rds: create data volume for %s: %w", in.DBInstanceIdentifier, volErr)
		}
		if volume == nil || aws.StringValue(volume.VolumeId) == "" {
			return nil, fmt.Errorf("rds: create data volume for %s: empty volume id", in.DBInstanceIdentifier)
		}
		volumeID = aws.StringValue(volume.VolumeId)
		volumeEncrypted = aws.BoolValue(volume.Encrypted)
		rollback = append(rollback, func(ctx context.Context) {
			if _, delErr := deps.Volume.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(volumeID)}, utils.GlobalAccountID); delErr != nil {
				slog.WarnContext(ctx, "rds: rollback delete of orphaned data volume failed",
					"dbInstance", in.DBInstanceIdentifier, "volumeId", volumeID, "err", delErr)
			}
		})

		// Checked before the attach so an unencrypted volume is unwound rather than
		// mounted: the cluster storage key is unset, which no retry here can fix.
		if !volumeEncrypted {
			return nil, fmt.Errorf("rds: data volume %s for %s was created unencrypted; the cluster storage key is not configured",
				volumeID, in.DBInstanceIdentifier)
		}
	}

	device, err := deps.Attacher.AttachVolume(ctx, utils.GlobalAccountID, instanceID, volumeID, dataVolumeDevice)
	if err != nil {
		return nil, fmt.Errorf("rds: attach data volume %s to %s: %w", volumeID, instanceID, err)
	}

	slog.InfoContext(ctx, "rds: DB VM launched",
		"dbInstance", in.DBInstanceIdentifier, "instanceId", instanceID, "ami", amiID,
		"systemEni", systemENI.id, "systemSg", systemSGID,
		"customerEni", customerENI.id, "customerEniIp", customerENI.ip,
		"dataVolume", volumeID, "device", device)

	return &LaunchOutput{
		InstanceID:          instanceID,
		SystemENIID:         systemENI.id,
		SystemSGID:          systemSGID,
		CustomerENIID:       customerENI.id,
		CustomerENIIP:       customerENI.ip,
		DataVolumeID:        volumeID,
		DataVolumeSerial:    vm.VolumeSerial(volumeID),
		DataVolumeEncrypted: volumeEncrypted,
		CreatedDataVolume:   in.ExistingDataVolume == "",
		Unwind:              unwind,
	}, nil
}

func validateLaunchInput(in LaunchInput) error {
	switch {
	case in.DBInstanceIdentifier == "":
		return errors.New("rds: LaunchDBInstanceVM empty db instance identifier")
	case in.AccountID == "":
		return errors.New("rds: LaunchDBInstanceVM empty account id")
	case in.SubnetID == "":
		return errors.New("rds: LaunchDBInstanceVM empty subnet id")
	case in.InstanceType == "":
		return errors.New("rds: LaunchDBInstanceVM empty instance type")
	case in.AllocatedStorage <= 0:
		return errors.New("rds: LaunchDBInstanceVM non-positive allocated storage")
	}
	return nil
}

type launchENI struct {
	id  string
	ip  string
	mac string
}

// groups must be non-empty for either NIC: an ENI created with none is placed in
// its VPC's default group, whose sole ingress rule admits every other member of
// itself — which for the shared system VPC is every DB VM in the deployment.
// suppressDHCP marks the customer endpoint ENI, which is always statically
// addressed; the system/primary NIC keeps DHCP for IMDS bootstrap.
func createLaunchENI(ctx context.Context, vpcSvc launchVPCProvisioner, accountID, subnetID string, groups []string, description, dbInstanceID string, suppressDHCP bool) (*launchENI, error) {
	var groupIDs []*string
	if len(groups) > 0 {
		groupIDs = aws.StringSlice(groups)
	}
	eniTags := []*ec2.Tag{
		{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByRDS)},
		{Key: aws.String(rdsInstanceTagKey), Value: aws.String(dbInstanceID)},
	}
	if suppressDHCP {
		eniTags = append(eniTags, &ec2.Tag{Key: aws.String(tags.DHCPDisabledKey), Value: aws.String(tags.DHCPDisabledValue)})
	}
	out, err := vpcSvc.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Description: aws.String(description),
		Groups:      groupIDs,
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("network-interface"),
			Tags:         eniTags,
		}},
	}, accountID)
	if err != nil {
		return nil, fmt.Errorf("rds: create ENI in subnet %s: %w", subnetID, err)
	}
	if out == nil || out.NetworkInterface == nil ||
		aws.StringValue(out.NetworkInterface.NetworkInterfaceId) == "" ||
		aws.StringValue(out.NetworkInterface.PrivateIpAddress) == "" {
		return nil, fmt.Errorf("rds: create ENI in subnet %s: incomplete interface returned", subnetID)
	}
	ni := out.NetworkInterface
	return &launchENI{
		id:  aws.StringValue(ni.NetworkInterfaceId),
		ip:  aws.StringValue(ni.PrivateIpAddress),
		mac: aws.StringValue(ni.MacAddress),
	}, nil
}

// The customer NIC this launch attaches: a fresh one for a create, the DB
// instance's persisted one for a replace. The persisted case is read back
// rather than reconstructed, because the launcher needs the MAC and only the
// address is on the record.
func resolveCustomerENI(ctx context.Context, vpcSvc launchVPCProvisioner, in LaunchInput) (*launchENI, error) {
	if in.ExistingCustomerENI == "" {
		return createLaunchENI(ctx, vpcSvc, in.AccountID, in.SubnetID, in.SecurityGroupIDs,
			"RDS endpoint ENI for "+in.DBInstanceIdentifier, in.DBInstanceIdentifier, true)
	}

	// The old VM is gone by now but its attachment record can outlive it, and a
	// re-attach of an ENI that still reads as attached is rejected.
	if err := vpcSvc.DetachENI(ctx, in.AccountID, in.ExistingCustomerENI); err != nil && !awserrors.IsNotFound(err) {
		return nil, fmt.Errorf("rds: detach the endpoint ENI %s before re-attaching it: %w", in.ExistingCustomerENI, err)
	}

	out, err := vpcSvc.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: aws.StringSlice([]string{in.ExistingCustomerENI}),
	}, in.AccountID)
	if err != nil {
		return nil, fmt.Errorf("rds: read the endpoint ENI %s: %w", in.ExistingCustomerENI, err)
	}
	for _, ni := range out.NetworkInterfaces {
		if aws.StringValue(ni.NetworkInterfaceId) != in.ExistingCustomerENI {
			continue
		}
		eni := &launchENI{
			id:  in.ExistingCustomerENI,
			ip:  aws.StringValue(ni.PrivateIpAddress),
			mac: aws.StringValue(ni.MacAddress),
		}
		if eni.ip == "" || eni.mac == "" {
			return nil, fmt.Errorf("rds: endpoint ENI %s has no address or MAC to re-attach", in.ExistingCustomerENI)
		}
		return eni, nil
	}
	// Failing here is the only safe answer: minting a replacement would move the
	// endpoint to a new address the DNS record and serving cert do not name.
	return nil, fmt.Errorf("rds: endpoint ENI %s no longer exists", in.ExistingCustomerENI)
}

// customerENISubnetGateway resolves the customer subnet's prefix length and
// gateway IP (network address +1), matching OVN's per-subnet router
// placement (see network/topology.SubnetGatewayCIDR). Both are needed to
// configure the customer ENI statically instead of via DHCP.
func customerENISubnetGateway(ctx context.Context, vpcSvc launchVPCProvisioner, accountID, subnetID string) (prefixBits int, gateway string, err error) {
	out, err := vpcSvc.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		SubnetIds: aws.StringSlice([]string{subnetID}),
	}, accountID)
	if err != nil {
		return 0, "", fmt.Errorf("rds: describe subnet %s: %w", subnetID, err)
	}
	if out == nil {
		return 0, "", fmt.Errorf("rds: subnet %s not found", subnetID)
	}
	for _, subnet := range out.Subnets {
		if aws.StringValue(subnet.SubnetId) != subnetID {
			continue
		}
		_, ipNet, perr := net.ParseCIDR(aws.StringValue(subnet.CidrBlock))
		if perr != nil {
			return 0, "", fmt.Errorf("rds: subnet %s has an unparseable CIDR %q: %w",
				subnetID, aws.StringValue(subnet.CidrBlock), perr)
		}
		ones, _ := ipNet.Mask.Size()
		gw := ipNet.IP.To4()
		if gw == nil {
			return 0, "", fmt.Errorf("rds: subnet %s CIDR %q is not IPv4", subnetID, ipNet.String())
		}
		gwCopy := make(net.IP, len(gw))
		copy(gwCopy, gw)
		gwCopy[3]++
		return ones, gwCopy.String(), nil
	}
	return 0, "", fmt.Errorf("rds: subnet %s not found", subnetID)
}

// Detach comes first because a terminated VM can leave the attachment record
// behind, which a plain delete rejects as InUse.
func deleteLaunchENI(ctx context.Context, vpcSvc launchVPCProvisioner, accountID, eniID string) {
	if err := vpcSvc.DetachENI(ctx, accountID, eniID); err != nil && !awserrors.IsNotFound(err) {
		slog.DebugContext(ctx, "rds: rollback ENI detach failed", "eniId", eniID, "err", err)
	}
	if _, err := vpcSvc.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniID),
	}, accountID); err != nil && !awserrors.IsNotFound(err) {
		slog.WarnContext(ctx, "rds: rollback delete of orphaned ENI failed", "eniId", eniID, "err", err)
	}
}

// An empty version takes the newest image; a set one requires an exact match,
// so a request can never be served by a different major version.
func resolveEngineAMI(ctx context.Context, amiSvc launchAMIResolver, engine, version string) (string, error) {
	filters := []*ec2.Filter{
		{Name: aws.String("tag:" + tags.ManagedByKey), Values: aws.StringSlice([]string{tags.ManagedByRDS})},
		{Name: aws.String("tag:" + engineTagKey), Values: aws.StringSlice([]string{engine})},
		{Name: aws.String("tag:" + dataVolumeContractTagKey), Values: aws.StringSlice([]string{dataVolumeContractV1})},
	}
	if version != "" {
		filters = append(filters, &ec2.Filter{
			Name:   aws.String("tag:" + engineVersionTagKey),
			Values: aws.StringSlice([]string{version}),
		})
	}

	out, err := amiSvc.DescribeImages(ctx, &ec2.DescribeImagesInput{Filters: filters}, utils.GlobalAccountID)
	if err != nil {
		return "", fmt.Errorf("rds: describe %s AMI: %w", engine, err)
	}
	if out == nil {
		return "", engineAMINotFound(engine, version)
	}

	// Several builds of one engine version can be registered; select the most
	// recently imported usable image. A GPU engine build carries the same engine
	// tags, so it is excluded or a newer GPU image would hijack an ordinary instance.
	newestID, _, matches := utils.SelectNewestImage(out.Images, tags.GPUVendorKey)
	if newestID == "" {
		return "", engineAMINotFound(engine, version)
	}
	if matches > 1 {
		slog.WarnContext(ctx, "rds: multiple AMIs match the requested engine; using newest",
			"engine", engine, "engineVersion", version, "imageId", newestID, "matches", matches)
	}
	return newestID, nil
}

// Only the node running the VM subscribes the per-instance ec2.cmd subject, so
// the command routes itself and the caller need not know where the VM landed.
type natsVolumeAttacher struct {
	nc      *nats.Conn
	timeout time.Duration
}

var _ volumeAttacher = (*natsVolumeAttacher)(nil)

func NewNATSVolumeAttacher(nc *nats.Conn) volumeAttacher {
	return &natsVolumeAttacher{nc: nc, timeout: attachRequestTimeout}
}

// Returns the device the attachment landed on, which can differ from the
// requested one when the guest renames it.
func (a *natsVolumeAttacher) AttachVolume(ctx context.Context, accountID, instanceID, volumeID, device string) (string, error) {
	cmd := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{AttachVolume: true},
		AttachVolumeData: &types.AttachVolumeData{
			VolumeID: volumeID,
			Device:   device,
		},
	}
	out, err := utils.NATSRequest[ec2.VolumeAttachment](ctx, a.nc,
		"ec2.cmd."+instanceID, cmd, a.timeout, accountID)
	if err != nil {
		if !errors.Is(err, nats.ErrNoResponders) {
			return "", err
		}
		stopped, lookupErr := a.isStoppedInstance(ctx, accountID, instanceID)
		if lookupErr != nil {
			slog.ErrorContext(ctx, "rds: classify attach no-responder",
				"instanceId", instanceID, "volumeId", volumeID, "err", lookupErr)
			return "", errors.New(awserrors.ErrorServerInternal)
		}
		if stopped {
			return "", errors.New(awserrors.ErrorIncorrectInstanceState)
		}
		return "", errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	}
	if out == nil {
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	attachedDevice := aws.StringValue(out.Device)
	if attachedDevice == "" {
		slog.ErrorContext(ctx, "rds: attach volume returned no device",
			"instanceId", instanceID, "volumeId", volumeID)
		return "", errors.New(awserrors.ErrorServerInternal)
	}
	return attachedDevice, nil
}

// No responder can mean either a stopped instance or an ID no node owns. The
// shared stopped-instance lookup disambiguates those AWS errors for the caller.
func (a *natsVolumeAttacher) isStoppedInstance(ctx context.Context, accountID, instanceID string) (bool, error) {
	input := ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String(instanceID)}}
	out, err := utils.NATSRequest[ec2.DescribeInstancesOutput](ctx, a.nc,
		"ec2.DescribeStoppedInstances", &input, 3*time.Second, accountID)
	if err != nil {
		return false, err
	}
	if out == nil {
		return false, errors.New("stopped-instance lookup returned no response")
	}
	for _, reservation := range out.Reservations {
		if len(reservation.Instances) > 0 {
			return true, nil
		}
	}
	return false, nil
}
