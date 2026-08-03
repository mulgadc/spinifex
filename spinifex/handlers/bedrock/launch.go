package handlers_bedrock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// vllmServePort is the vLLM OpenAI-compatible server's listen port, baked
// into the vllm-serving AMI's systemd unit. Both the readiness probe and
// real inference traffic (from the awsgw process) dial this port on the
// serving VM's system-VPC ENI private IP.
const vllmServePort = 8000

// weightsVolumeDevice is where a serving VM's COW-cloned weights volume is
// attached. Fixed like RDS's dataVolumeDevice: the guest locates it by
// device, not by name, and one VM never carries more than one weights volume.
const weightsVolumeDevice = "/dev/sdf"

// bedrockInstanceTagKey links an ENI or volume back to its serving VM, so a
// teardown or leak sweep can find them without the KV record.
const bedrockInstanceTagKey = "spinifex:bedrock-endpoint"

// A fresh budget rather than the caller's remainder: the unwind is often
// triggered by that deadline expiring, and a dead context deletes nothing.
const rollbackTimeout = 60 * time.Second

// ErrNoWeightsStaged is returned when no weights snapshot has been staged for
// a model that otherwise has a serving spec — an operator must PutWeights
// before the model is servable.
var ErrNoWeightsStaged = errors.New("bedrock: no weights snapshot staged for this model")

type launchVPCProvisioner interface {
	CreateNetworkInterface(ctx context.Context, input *ec2.CreateNetworkInterfaceInput, accountID string) (*ec2.CreateNetworkInterfaceOutput, error)
	DeleteNetworkInterface(ctx context.Context, input *ec2.DeleteNetworkInterfaceInput, accountID string) (*ec2.DeleteNetworkInterfaceOutput, error)
	DetachENI(ctx context.Context, accountID, eniID string) error
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
	Instance  sysinstance.SystemInstanceLauncher
	Image     amiResolver
	Volume    launchVolumeProvisioner
	Attacher  volumeAttacher
	Weights   gateway_bedrock.WeightsResolver
}

// LaunchInput describes the serving VM to launch. Everything here is already
// validated and admission-checked by the caller (Service.Ensure): the launch
// helper resolves and wires, it does not re-police capacity or catalog rules.
type LaunchInput struct {
	ModelID      string
	InstanceType string
	VLLMArgs     []string
}

type LaunchOutput struct {
	InstanceID      string
	ENIID           string
	PrivateIP       string
	WeightsVolumeID string
	// BaseURL is the vLLM server's base address, derived from PrivateIP.
	BaseURL string
	// Tears down everything this launch created, for a caller whose readiness
	// probe times out after it returned. The launch runs it itself on its own
	// failures, so it is only ever invoked once.
	Unwind func(context.Context)
}

// LaunchServingVM boots one self-hosted vLLM serving VM for in.ModelID. On any
// failure every resource this call created is torn down, so a retried Ensure
// does not accumulate orphan ENIs, volumes and VMs.
func LaunchServingVM(ctx context.Context, deps LaunchDeps, in LaunchInput) (out *LaunchOutput, err error) {
	if err := validateLaunchInput(in); err != nil {
		return nil, err
	}
	region, az := deps.Config.Region, deps.Config.AZ

	amiID, err := resolveServingAMI(ctx, deps.Image)
	if err != nil {
		return nil, err
	}

	snapshotID, resolvable, err := deps.Weights.Resolve(ctx, in.ModelID)
	if err != nil {
		return nil, fmt.Errorf("bedrock: resolve weights for %s: %w", in.ModelID, err)
	}
	if !resolvable {
		return nil, fmt.Errorf("%w: %s", ErrNoWeightsStaged, in.ModelID)
	}

	sysRefs, err := EnsureSystemVPC(ctx, deps.SystemVPC, &deps.Config.Bedrock, utils.GlobalAccountID, region)
	if err != nil {
		return nil, err
	}
	systemSubnetID := sysRefs.PrivateSubnetIDs[0]

	// Unwind in reverse creation order on any failure below. Each step appends
	// its own undo as soon as the resource exists.
	var rollback []func(context.Context)

	// The VM is torn down first regardless of creation order: the ENI and
	// weights volume are attached while it runs, and deleting those is
	// rejected as InUse.
	var terminateVM func(context.Context)

	// A context detached from the caller's, so a failure caused by the
	// caller's own deadline expiring can still clean up.
	unwind := func(ctx context.Context) {
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		if terminateVM != nil {
			terminateVM(rbCtx)
		}
		for _, v := range slices.Backward(rollback) {
			v(rbCtx)
		}
	}
	defer func() {
		if err != nil {
			unwind(ctx)
		}
	}()

	eni, err := createLaunchENI(ctx, deps.VPC, systemSubnetID, in.ModelID)
	if err != nil {
		return nil, err
	}
	rollback = append(rollback, func(ctx context.Context) {
		deleteLaunchENI(ctx, deps.VPC, eni.id)
	})

	weightsVolumeID, err := createWeightsVolume(ctx, deps.Volume, az, snapshotID, in.ModelID)
	if err != nil {
		return nil, err
	}
	rollback = append(rollback, func(ctx context.Context) {
		if _, delErr := deps.Volume.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(weightsVolumeID)}, utils.GlobalAccountID); delErr != nil {
			slog.WarnContext(ctx, "bedrock: rollback delete of orphaned weights volume failed",
				"model", in.ModelID, "volumeId", weightsVolumeID, "err", delErr)
		}
	})

	userData := buildServeUserData(serveUserDataInput{
		ModelID:       in.ModelID,
		VLLMArgs:      in.VLLMArgs,
		WeightsDevice: weightsVolumeDevice,
		ServePort:     vllmServePort,
	})

	sysOut, err := deps.Instance.LaunchSystemInstance(&sysinstance.SystemInstanceInput{
		BootMode:     sysinstance.BootAMI,
		ManagedBy:    tags.ManagedByBedrock,
		InstanceType: in.InstanceType,
		ImageID:      amiID,
		// The VM and its primary NIC live in the system account: a serving VM
		// has no customer owner of its own, only the accountID the endpoint
		// record is keyed under (a separate, KV-level concern from VM ownership).
		AccountID: utils.GlobalAccountID,
		SubnetID:  systemSubnetID,
		ENIID:     eni.id,
		ENIMac:    eni.mac,
		ENIIP:     eni.ip,
		UserData:  userData,
	})
	if err != nil {
		return nil, fmt.Errorf("bedrock: launch serving VM for %s: %w", in.ModelID, err)
	}
	if sysOut == nil || sysOut.InstanceID == "" {
		return nil, fmt.Errorf("bedrock: launch serving VM for %s: launcher returned no instance", in.ModelID)
	}
	instanceID := sysOut.InstanceID
	terminateVM = func(ctx context.Context) {
		if termErr := deps.Instance.TerminateSystemInstance(instanceID); termErr != nil &&
			!errors.Is(termErr, sysinstance.ErrSystemInstanceNotFound) {
			slog.WarnContext(ctx, "bedrock: rollback terminate of failed serving VM failed",
				"model", in.ModelID, "instanceId", instanceID, "err", termErr)
		}
	}

	device, err := deps.Attacher.AttachVolume(ctx, utils.GlobalAccountID, instanceID, weightsVolumeID, weightsVolumeDevice)
	if err != nil {
		return nil, fmt.Errorf("bedrock: attach weights volume %s to %s: %w", weightsVolumeID, instanceID, err)
	}

	slog.InfoContext(ctx, "bedrock: serving VM launched",
		"model", in.ModelID, "instanceId", instanceID, "ami", amiID,
		"eni", eni.id, "eniIp", eni.ip, "weightsVolume", weightsVolumeID, "device", device)

	return &LaunchOutput{
		InstanceID:      instanceID,
		ENIID:           eni.id,
		PrivateIP:       eni.ip,
		WeightsVolumeID: weightsVolumeID,
		BaseURL:         "http://" + net.JoinHostPort(eni.ip, strconv.Itoa(vllmServePort)),
		Unwind:          unwind,
	}, nil
}

// TerminateServingVM tears down a DRAINING endpoint's VM, ENI and weights
// volume. The VM is terminated first (ENI/volume deletes would otherwise be
// rejected as InUse), and every step is best-effort and logged rather than
// aborted on the first failure, so one stuck resource does not block the
// others from being reclaimed. The caller decides whether the returned error
// should block the KV record's DRAINING->ABSENT transition.
func TerminateServingVM(ctx context.Context, deps LaunchDeps, rec EndpointRecord) error {
	var errs []error
	if rec.InstanceID != "" {
		if err := deps.Instance.TerminateSystemInstance(rec.InstanceID); err != nil &&
			!errors.Is(err, sysinstance.ErrSystemInstanceNotFound) {
			errs = append(errs, fmt.Errorf("terminate instance %s: %w", rec.InstanceID, err))
		}
	}
	if rec.ENIID != "" {
		deleteLaunchENI(ctx, deps.VPC, rec.ENIID)
	}
	if rec.WeightsVolumeID != "" {
		if _, err := deps.Volume.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(rec.WeightsVolumeID)}, utils.GlobalAccountID); err != nil &&
			!awserrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete weights volume %s: %w", rec.WeightsVolumeID, err))
		}
	}
	return errors.Join(errs...)
}

func validateLaunchInput(in LaunchInput) error {
	switch {
	case in.ModelID == "":
		return errors.New("bedrock: LaunchServingVM empty model id")
	case in.InstanceType == "":
		return errors.New("bedrock: LaunchServingVM empty instance type")
	}
	return nil
}

type launchENI struct {
	id  string
	ip  string
	mac string
}

// The system NIC is unreachable from any customer VPC — a serving VM has no
// customer-facing surface — so it needs no security group of its own.
func createLaunchENI(ctx context.Context, vpcSvc launchVPCProvisioner, subnetID, modelID string) (*launchENI, error) {
	out, err := vpcSvc.CreateNetworkInterface(ctx, &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Description: aws.String("Bedrock serving NIC for " + modelID),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("network-interface"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByBedrock)},
				{Key: aws.String(bedrockInstanceTagKey), Value: aws.String(modelID)},
			},
		}},
	}, utils.GlobalAccountID)
	if err != nil {
		return nil, fmt.Errorf("bedrock: create ENI in subnet %s: %w", subnetID, err)
	}
	if out == nil || out.NetworkInterface == nil ||
		aws.StringValue(out.NetworkInterface.NetworkInterfaceId) == "" ||
		aws.StringValue(out.NetworkInterface.PrivateIpAddress) == "" {
		return nil, fmt.Errorf("bedrock: create ENI in subnet %s: incomplete interface returned", subnetID)
	}
	ni := out.NetworkInterface
	return &launchENI{
		id:  aws.StringValue(ni.NetworkInterfaceId),
		ip:  aws.StringValue(ni.PrivateIpAddress),
		mac: aws.StringValue(ni.MacAddress),
	}, nil
}

// Detach comes first because a terminated VM can leave the attachment record
// behind, which a plain delete rejects as InUse.
func deleteLaunchENI(ctx context.Context, vpcSvc launchVPCProvisioner, eniID string) {
	if err := vpcSvc.DetachENI(ctx, utils.GlobalAccountID, eniID); err != nil && !awserrors.IsNotFound(err) {
		slog.DebugContext(ctx, "bedrock: rollback ENI detach failed", "eniId", eniID, "err", err)
	}
	if _, err := vpcSvc.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(eniID),
	}, utils.GlobalAccountID); err != nil && !awserrors.IsNotFound(err) {
		slog.WarnContext(ctx, "bedrock: rollback delete of orphaned ENI failed", "eniId", eniID, "err", err)
	}
}

// createWeightsVolume COW-clones modelID's staged weights snapshot into a
// fresh volume this endpoint's VM attaches — Phase-0's bake-into-a-volume,
// CreateSnapshot, COW-clone-per-endpoint resolution.
func createWeightsVolume(ctx context.Context, volSvc launchVolumeProvisioner, az, snapshotID, modelID string) (string, error) {
	volume, err := volSvc.CreateVolume(ctx, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(az),
		SnapshotId:       aws.String(snapshotID),
		VolumeType:       aws.String("gp3"),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("volume"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByBedrock)},
				{Key: aws.String(bedrockInstanceTagKey), Value: aws.String(modelID)},
			},
		}},
	}, utils.GlobalAccountID)
	if err != nil {
		return "", fmt.Errorf("bedrock: clone weights volume for %s from snapshot %s: %w", modelID, snapshotID, err)
	}
	if volume == nil || aws.StringValue(volume.VolumeId) == "" {
		return "", fmt.Errorf("bedrock: clone weights volume for %s: empty volume id", modelID)
	}
	return aws.StringValue(volume.VolumeId), nil
}
