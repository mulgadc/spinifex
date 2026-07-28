package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
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
	"github.com/nats-io/nats.go"
)

// A DB instance is a dual-NIC system VM. Its primary NIC lives in the shared
// RDS system VPC, giving the in-guest agent egress to the gateway; the
// customer-facing ENI is injected cross-account as a pure ingress path. There
// is no single-NIC mode, since DB subnets typically have no NAT egress.
//
// The data volume is created and hot-plug attached after the VM is running, and
// kept separate from the boot volume so a replace can discard the boot volume
// and keep the datadir.

const (
	// dataVolumeDevice is the API-form device name the data volume attaches at.
	// The guest locates it by volume serial rather than by this name, but a
	// fixed name keeps the attachment record predictable across replaces.
	dataVolumeDevice = "/dev/sdf"

	// rdsInstanceTagKey links an ENI or volume back to the DB instance that owns
	// it, so a teardown or leak sweep can find them without the KV record.
	rdsInstanceTagKey = "spinifex:rds-db-instance"

	// engineTagKey and engineVersionTagKey are stamped on the engine AMIs by
	// their image manifest, and are what an EngineVersion request resolves
	// against.
	engineTagKey        = "engine"
	engineVersionTagKey = "engine-version"

	// attachRequestTimeout bounds the data-volume attach round-trip, matching
	// the budget the gateway's AttachVolume gives the same QMP pipeline.
	attachRequestTimeout = 30 * time.Second

	// rollbackTimeout bounds the unwind. A fresh budget rather than the caller's
	// remainder, since the unwind is often triggered by that deadline expiring
	// and a rollback on a dead context deletes nothing.
	rollbackTimeout = 60 * time.Second
)

// ErrEngineAMINotFound is returned when no AMI carries the requested engine's
// tags. A DB VM must never fall back to another image, so callers surface it as
// a validation failure rather than launching something else.
var ErrEngineAMINotFound = errors.New("rds: no AMI found for engine")

// launchVPCProvisioner is the ENI surface the launch helper needs, in both the
// system account (primary NIC) and the customer account (customer-facing ENI).
type launchVPCProvisioner interface {
	CreateNetworkInterface(ctx context.Context, input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error)
	DeleteNetworkInterface(ctx context.Context, input *ec2.DeleteNetworkInterfaceInput, accountID string) (*ec2.DeleteNetworkInterfaceOutput, error)
	DetachENI(ctx context.Context, accountID, eniID string) error
}

// launchInstanceLauncher is the system-instance surface the DB VM boots through.
// System-managed VMs get a mgmt-bridge NIC alongside their VPC NICs, which is
// how the agent reaches the gateway from a private subnet.
type launchInstanceLauncher interface {
	LaunchSystemInstance(input *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error)
	TerminateSystemInstance(instanceID string) error
}

// launchAMIResolver is the narrow AMI surface for resolving the engine image.
type launchAMIResolver interface {
	DescribeImages(ctx context.Context, input *ec2.DescribeImagesInput, accountID string) (*ec2.DescribeImagesOutput, error)
}

// launchVolumeProvisioner creates and removes the decoupled data volume.
type launchVolumeProvisioner interface {
	CreateVolume(ctx context.Context, input *ec2.CreateVolumeInput, accountID string) (*ec2.Volume, error)
	DeleteVolume(ctx context.Context, input *ec2.DeleteVolumeInput, accountID string) (*ec2.DeleteVolumeOutput, error)
}

// volumeAttacher hot-plugs a volume onto a running VM. Split from the volume
// provisioner because attach is routed to the node owning the VM rather than
// answered by whichever node picks the request up.
type volumeAttacher interface {
	AttachVolume(ctx context.Context, accountID, instanceID, volumeID, device string) (string, error)
}

// LaunchDeps bundles the collaborators LaunchDBInstanceVM composes.
type LaunchDeps struct {
	Config    *config.Config
	SystemVPC handlers_systemvpc.Deps
	VPC       launchVPCProvisioner
	Instance  launchInstanceLauncher
	Image     launchAMIResolver
	Volume    launchVolumeProvisioner
	Attacher  volumeAttacher
}

// LaunchInput describes one DB VM to boot. Everything here is already validated
// by the caller: the launch helper resolves and wires, it does not police the
// AWS API surface.
type LaunchInput struct {
	DBInstanceIdentifier string
	// AccountID is the customer account that owns the DB, the subnet and the
	// customer-facing ENI. The VM itself runs in the system account.
	AccountID string
	// SubnetID is the customer DB-subnet-group subnet the customer ENI lands in.
	SubnetID string
	// SecurityGroupIDs gate ingress on that ENI; they are the customer's.
	SecurityGroupIDs []string
	Engine           string
	EngineVersion    string
	// InstanceType is the EC2 type the db.* class resolved to.
	InstanceType string
	// AllocatedStorage is the data volume size in GiB.
	AllocatedStorage int64
	// UserData is the cloud-init blob carrying the agent's gateway endpoint.
	UserData string
	// IamInstanceProfileArn attaches the rdsInstanceRole so the agent signs with
	// IMDS role credentials rather than a static secret in user-data.
	IamInstanceProfileArn string
}

// LaunchOutput is what a booted DB VM is composed of. The caller persists the
// customer ENI and data volume on the DBInstance record: both outlive the VM.
type LaunchOutput struct {
	InstanceID string
	// SystemENIID is the primary NIC in the RDS system VPC. It is disposable —
	// a VM replace makes a new one.
	SystemENIID string
	// CustomerENIID is the stable endpoint: its private IP is the DNS target
	// and survives a VM replace.
	CustomerENIID  string
	CustomerENIIP  string
	CustomerENIMac string
	DataVolumeID   string
	DataDevice     string
	MgmtIP         string
}

// LaunchDBInstanceVM boots the dual-NIC DB VM for in and attaches its data
// volume. On any failure every resource this call created is torn down, so a
// retried create does not accumulate orphan ENIs, volumes and VMs.
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

	// Unwind in reverse creation order on any failure below. Each step appends
	// its own undo as soon as the resource exists.
	var rollback []func(context.Context)

	// The VM is torn down first regardless of creation order: the ENIs and data
	// volume are attached while it runs, and deleting those is rejected as InUse.
	var terminateVM func(context.Context)

	defer func() {
		if err == nil {
			return
		}
		// A context detached from the caller's, so a create that failed *because*
		// the request deadline expired can still clean up.
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		if terminateVM != nil {
			terminateVM(rbCtx)
		}
		for _, undo := range slices.Backward(rollback) {
			undo(rbCtx)
		}
	}()

	systemENI, err := createLaunchENI(ctx, deps.VPC, utils.GlobalAccountID, systemSubnetID, nil,
		"RDS management NIC for "+in.DBInstanceIdentifier, in.DBInstanceIdentifier)
	if err != nil {
		return nil, err
	}
	rollback = append(rollback, func(ctx context.Context) {
		deleteLaunchENI(ctx, deps.VPC, utils.GlobalAccountID, systemENI.id)
	})

	customerENI, err := createLaunchENI(ctx, deps.VPC, in.AccountID, in.SubnetID, in.SecurityGroupIDs,
		"RDS endpoint ENI for "+in.DBInstanceIdentifier, in.DBInstanceIdentifier)
	if err != nil {
		return nil, err
	}
	rollback = append(rollback, func(ctx context.Context) {
		deleteLaunchENI(ctx, deps.VPC, in.AccountID, customerENI.id)
	})

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
			ENIID:     customerENI.id,
			ENIMac:    customerENI.mac,
			ENIIP:     customerENI.ip,
			SubnetID:  in.SubnetID,
			AccountID: in.AccountID,
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

	// The data volume is created in the system account alongside the VM: it is
	// attached to a system-account instance, and the customer reaches its
	// contents only through the engine.
	volume, err := deps.Volume.CreateVolume(ctx, &ec2.CreateVolumeInput{
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
	if err != nil {
		return nil, fmt.Errorf("rds: create data volume for %s: %w", in.DBInstanceIdentifier, err)
	}
	if volume == nil || aws.StringValue(volume.VolumeId) == "" {
		return nil, fmt.Errorf("rds: create data volume for %s: empty volume id", in.DBInstanceIdentifier)
	}
	volumeID := aws.StringValue(volume.VolumeId)
	rollback = append(rollback, func(ctx context.Context) {
		if _, delErr := deps.Volume.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(volumeID)}, utils.GlobalAccountID); delErr != nil {
			slog.WarnContext(ctx, "rds: rollback delete of orphaned data volume failed",
				"dbInstance", in.DBInstanceIdentifier, "volumeId", volumeID, "err", delErr)
		}
	})

	device, err := deps.Attacher.AttachVolume(ctx, utils.GlobalAccountID, instanceID, volumeID, dataVolumeDevice)
	if err != nil {
		return nil, fmt.Errorf("rds: attach data volume %s to %s: %w", volumeID, instanceID, err)
	}

	slog.InfoContext(ctx, "rds: DB VM launched",
		"dbInstance", in.DBInstanceIdentifier, "instanceId", instanceID, "ami", amiID,
		"systemEni", systemENI.id, "customerEni", customerENI.id, "customerEniIp", customerENI.ip,
		"dataVolume", volumeID, "device", device)

	return &LaunchOutput{
		InstanceID:     instanceID,
		SystemENIID:    systemENI.id,
		CustomerENIID:  customerENI.id,
		CustomerENIIP:  customerENI.ip,
		CustomerENIMac: customerENI.mac,
		DataVolumeID:   volumeID,
		DataDevice:     device,
		MgmtIP:         sysOut.MgmtIP,
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

// launchENI is a created interface's identity. The launcher needs all three:
// the guest's NIC is configured from the MAC and IP.
type launchENI struct {
	id  string
	ip  string
	mac string
}

// createLaunchENI creates one of the DB VM's two NICs in accountID's subnet.
// groups is nil for the system NIC, which is unreachable from any customer VPC
// and so needs no security group of its own.
func createLaunchENI(ctx context.Context, vpcSvc launchVPCProvisioner, accountID, subnetID string, groups []string, description, dbInstanceID string) (*launchENI, error) {
	var groupIDs []*string
	if len(groups) > 0 {
		groupIDs = aws.StringSlice(groups)
	}
	out, err := vpcSvc.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Description: aws.String(description),
		Groups:      groupIDs,
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("network-interface"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByRDS)},
				{Key: aws.String(rdsInstanceTagKey), Value: aws.String(dbInstanceID)},
			},
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

// deleteLaunchENI removes an ENI during rollback. The detach comes first because
// a terminated VM can leave the attachment record behind, which a plain delete
// rejects as InUse. Best-effort: the caller is already unwinding a failure.
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

// resolveEngineAMI picks the engine's system AMI by its manifest tags. An empty
// version takes the newest image; a set one requires an exact match, so a
// request can never be served by a different major version.
func resolveEngineAMI(ctx context.Context, amiSvc launchAMIResolver, engine, version string) (string, error) {
	filters := []*ec2.Filter{
		{Name: aws.String("tag:" + tags.ManagedByKey), Values: aws.StringSlice([]string{tags.ManagedByRDS})},
		{Name: aws.String("tag:" + engineTagKey), Values: aws.StringSlice([]string{engine})},
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
	if out == nil || len(out.Images) == 0 {
		return "", fmt.Errorf("%w: %s %s", ErrEngineAMINotFound, engine, version)
	}

	// Several builds of one engine version can be registered; the newest is the
	// one an operator most recently imported.
	images := out.Images
	sort.Slice(images, func(i, j int) bool {
		return aws.StringValue(images[i].CreationDate) > aws.StringValue(images[j].CreationDate)
	})
	imageID := aws.StringValue(images[0].ImageId)
	if imageID == "" {
		return "", fmt.Errorf("%w: %s %s", ErrEngineAMINotFound, engine, version)
	}
	if len(images) > 1 {
		slog.WarnContext(ctx, "rds: multiple AMIs match the requested engine; using newest",
			"engine", engine, "engineVersion", version, "imageId", imageID, "matches", len(images))
	}
	return imageID, nil
}

// natsVolumeAttacher hot-plugs a volume through the per-instance ec2.cmd
// subject. Only the node running the VM subscribes it, so the command routes
// itself and the launch helper need not know where the VM landed.
type natsVolumeAttacher struct {
	nc      *nats.Conn
	timeout time.Duration
}

var _ volumeAttacher = (*natsVolumeAttacher)(nil)

// NewNATSVolumeAttacher builds the production volume attacher over nc.
func NewNATSVolumeAttacher(nc *nats.Conn) volumeAttacher {
	return &natsVolumeAttacher{nc: nc, timeout: attachRequestTimeout}
}

// AttachVolume returns the device name the attachment landed on, which can
// differ from the requested one when the guest renames it.
func (a *natsVolumeAttacher) AttachVolume(ctx context.Context, accountID, instanceID, volumeID, device string) (string, error) {
	cmd := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{AttachVolume: true},
		AttachVolumeData: &types.AttachVolumeData{
			VolumeID: volumeID,
			Device:   device,
		},
	}
	out, err := utils.NATSRequest[ec2.VolumeAttachment](ctx, a.nc, "ec2.cmd."+instanceID, cmd, a.timeout, accountID)
	if err != nil {
		return "", err
	}
	return aws.StringValue(out.Device), nil
}
