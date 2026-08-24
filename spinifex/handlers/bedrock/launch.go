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
	DescribeNetworkInterfaces(ctx context.Context, input *ec2.DescribeNetworkInterfacesInput, accountID string) (*ec2.DescribeNetworkInterfacesOutput, error)
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
	// HostPort installs this daemon's own port into the system VPC. Without it
	// nothing the launch produces is reachable, so a launch refuses to run.
	HostPort hostPortPlumber
	// NodeID names the daemon replica, so each node's system-VPC port is a
	// distinct ENI rather than one they contend for.
	NodeID string
}

// LaunchMemberInput describes one model the shared VM must serve: its own
// family (selecting the serving engine) and any extra engine args. The
// launcher assigns its port and weights device deterministically — the
// caller never has to pick around a collision.
type LaunchMemberInput struct {
	ModelID  string
	Family   string
	VLLMArgs []string
}

// LaunchInput describes the serving VM to launch: the bundle's group id (used
// for the ENI/volume tags, and identical to a standalone model's own model id
// in the bundle-of-one case), the instance type, and every member the shared
// VM must serve. Everything here is already validated and admission-checked
// by the caller (Service.Ensure): the launch helper resolves and wires, it
// does not re-police capacity or catalog rules.
type LaunchInput struct {
	GroupID      string
	InstanceType string
	Members      []LaunchMemberInput
}

// LaunchMemberOutput is one member's own address within the bundle's shared
// VM: its assigned port, the weights volume cloned for it, and the family
// (engine) it serves under, so a caller can pick that engine's own readiness
// route without a second catalog lookup.
type LaunchMemberOutput struct {
	Port            int
	WeightsVolumeID string
	Family          string
}

type LaunchOutput struct {
	InstanceID string
	ENIID      string
	PrivateIP  string
	// Members maps every launched member's model id to its own port and
	// weights volume within this VM.
	Members map[string]LaunchMemberOutput
	// PrimaryModelID is the bundle's generative (vLLM) member, or the sole
	// member for a bundle of one — the one EndpointRecord's BaseURL and
	// WeightsVolumeID mirror for callers that only care about the generative
	// model (the reaper's Prometheus scrape, the CLI's summary view).
	PrimaryModelID string
	// Tears down everything this launch created, for a caller whose readiness
	// probe times out after it returned. The launch runs it itself on its own
	// failures, so it is only ever invoked once.
	Unwind func(context.Context)
}

// MemberBaseURLs returns every member's own "http://ip:port" base address,
// keyed by model id.
func (o *LaunchOutput) MemberBaseURLs() map[string]string {
	out := make(map[string]string, len(o.Members))
	for modelID, m := range o.Members {
		out[modelID] = "http://" + net.JoinHostPort(o.PrivateIP, strconv.Itoa(m.Port))
	}
	return out
}

// MemberReadinessTargets returns every member's own base address paired with
// the path its engine's readiness must be probed on — what the multi-service
// readiness wait (waitReadyAll) polls.
func (o *LaunchOutput) MemberReadinessTargets() map[string]readinessTarget {
	out := make(map[string]readinessTarget, len(o.Members))
	for modelID, m := range o.Members {
		out[modelID] = readinessTarget{
			BaseURL: "http://" + net.JoinHostPort(o.PrivateIP, strconv.Itoa(m.Port)),
			Path:    readinessPath(m.Family),
		}
	}
	return out
}

// teiBasePort is the first port assigned to a non-vLLM (TEI) member; members
// beyond the first take the next port up. vllmServePort is never in this
// range, so a vLLM member's well-known port can never collide with one.
const teiBasePort = 8001

// assignMemberPorts assigns each member a deterministic serving port: the
// vLLM (familyMeta) member always takes vllmServePort, since that is the
// well-known port real inference traffic already dials; every other member
// takes the next port from teiBasePort up, in the order given. A bundle never
// carries more than one vLLM member — LookupCoServeGroup's catalog data
// guarantees that — so there is never a collision to resolve.
func assignMemberPorts(members []LaunchMemberInput) map[string]int {
	ports := make(map[string]int, len(members))
	next := teiBasePort
	for _, m := range members {
		if m.Family == gateway_bedrock.FamilyMeta {
			ports[m.ModelID] = vllmServePort
			continue
		}
		ports[m.ModelID] = next
		next++
	}
	return ports
}

// nthWeightsDevice returns the nth (0-based) member's weights-volume device
// name: /dev/sdf, then /dev/sdg, and so on. One VM never carries more than a
// handful of co-served members, so the alphabet is not a real ceiling.
func nthWeightsDevice(n int) string {
	return fmt.Sprintf("/dev/sd%c", 'f'+n)
}

// LaunchServingVM boots one shared self-hosted serving VM for every member in
// in.Members — a bundle of one is a standalone model launched exactly as
// before. On any failure every resource this call created is torn down, so a
// retried Ensure does not accumulate orphan ENIs, volumes and VMs.
func LaunchServingVM(ctx context.Context, deps LaunchDeps, in LaunchInput) (out *LaunchOutput, err error) {
	if err := validateLaunchInput(in); err != nil {
		return nil, err
	}
	region, az := deps.Config.Region, deps.Config.AZ

	amiID, err := resolveServingAMI(ctx, deps.Image)
	if err != nil {
		return nil, err
	}

	// Every member's weights must resolve before anything is created: a
	// partially-stageable bundle must refuse the whole launch, not boot a VM
	// that can only ever serve some of its members.
	snapshotIDs := make(map[string]string, len(in.Members))
	for _, m := range in.Members {
		snapshotID, resolvable, err := deps.Weights.Resolve(ctx, m.ModelID)
		if err != nil {
			return nil, fmt.Errorf("bedrock: resolve weights for %s: %w", m.ModelID, err)
		}
		if !resolvable {
			return nil, fmt.Errorf("%w: %s", ErrNoWeightsStaged, m.ModelID)
		}
		snapshotIDs[m.ModelID] = snapshotID
	}

	sysRefs, err := EnsureSystemVPC(ctx, deps.SystemVPC, &deps.Config.Bedrock, utils.GlobalAccountID, region)
	if err != nil {
		return nil, err
	}
	systemSubnetID := sysRefs.PrivateSubnetIDs[0]

	// Before anything is created: a VM this daemon cannot dial is worse than no
	// VM at all, since it holds a GPU for the whole readiness window first.
	if err := EnsureDaemonPort(ctx, deps, systemSubnetID, sysRefs.PrivateSubnetCIDRs[0]); err != nil {
		return nil, err
	}

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

	eni, err := createLaunchENI(ctx, deps.VPC, systemSubnetID, in.GroupID)
	if err != nil {
		return nil, err
	}
	rollback = append(rollback, func(ctx context.Context) {
		deleteLaunchENI(ctx, deps.VPC, eni.id)
	})

	ports := assignMemberPorts(in.Members)
	devices := make(map[string]string, len(in.Members))
	volumeIDs := make(map[string]string, len(in.Members))
	userDataMembers := make([]bundleMemberUserData, 0, len(in.Members))
	for i, m := range in.Members {
		device := nthWeightsDevice(i)
		devices[m.ModelID] = device

		weightsVolumeID, err := createWeightsVolume(ctx, deps.Volume, az, snapshotIDs[m.ModelID], m.ModelID)
		if err != nil {
			return nil, err
		}
		volumeIDs[m.ModelID] = weightsVolumeID
		rollback = append(rollback, func(ctx context.Context) {
			if _, delErr := deps.Volume.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(weightsVolumeID)}, utils.GlobalAccountID); delErr != nil {
				slog.WarnContext(ctx, "bedrock: rollback delete of orphaned weights volume failed",
					"model", m.ModelID, "volumeId", weightsVolumeID, "err", delErr)
			}
		})

		userDataMembers = append(userDataMembers, bundleMemberUserData{
			ModelID:       m.ModelID,
			Family:        m.Family,
			VLLMArgs:      m.VLLMArgs,
			WeightsDevice: device,
			Port:          ports[m.ModelID],
		})
	}

	userData := buildBundleUserData(bundleUserDataInput{
		GroupID: in.GroupID,
		Members: userDataMembers,
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
		return nil, fmt.Errorf("bedrock: launch serving VM for %s: %w", in.GroupID, err)
	}
	if sysOut == nil || sysOut.InstanceID == "" {
		return nil, fmt.Errorf("bedrock: launch serving VM for %s: launcher returned no instance", in.GroupID)
	}
	instanceID := sysOut.InstanceID
	terminateVM = func(ctx context.Context) {
		if termErr := deps.Instance.TerminateSystemInstance(instanceID); termErr != nil &&
			!errors.Is(termErr, sysinstance.ErrSystemInstanceNotFound) {
			slog.WarnContext(ctx, "bedrock: rollback terminate of failed serving VM failed",
				"group", in.GroupID, "instanceId", instanceID, "err", termErr)
		}
	}

	memberOut := make(map[string]LaunchMemberOutput, len(in.Members))
	primaryModelID := in.Members[0].ModelID
	for _, m := range in.Members {
		weightsVolumeID := volumeIDs[m.ModelID]
		attachedDevice, err := deps.Attacher.AttachVolume(ctx, utils.GlobalAccountID, instanceID, weightsVolumeID, devices[m.ModelID])
		if err != nil {
			return nil, fmt.Errorf("bedrock: attach weights volume %s to %s: %w", weightsVolumeID, instanceID, err)
		}
		memberOut[m.ModelID] = LaunchMemberOutput{Port: ports[m.ModelID], WeightsVolumeID: weightsVolumeID, Family: m.Family}
		if m.Family == gateway_bedrock.FamilyMeta {
			primaryModelID = m.ModelID
		}
		slog.InfoContext(ctx, "bedrock: member weights volume attached",
			"model", m.ModelID, "instanceId", instanceID, "device", attachedDevice)
	}

	slog.InfoContext(ctx, "bedrock: serving VM launched",
		"group", in.GroupID, "instanceId", instanceID, "ami", amiID,
		"eni", eni.id, "eniIp", eni.ip, "members", len(in.Members))

	return &LaunchOutput{
		InstanceID:     instanceID,
		ENIID:          eni.id,
		PrivateIP:      eni.ip,
		Members:        memberOut,
		PrimaryModelID: primaryModelID,
		Unwind:         unwind,
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
	for modelID, member := range rec.Members {
		if member.WeightsVolumeID == "" {
			continue
		}
		if _, err := deps.Volume.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(member.WeightsVolumeID)}, utils.GlobalAccountID); err != nil &&
			!awserrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete weights volume %s for %s: %w", member.WeightsVolumeID, modelID, err))
		}
	}
	return errors.Join(errs...)
}

func validateLaunchInput(in LaunchInput) error {
	switch {
	case in.GroupID == "":
		return errors.New("bedrock: LaunchServingVM empty group id")
	case in.InstanceType == "":
		return errors.New("bedrock: LaunchServingVM empty instance type")
	case len(in.Members) == 0:
		return errors.New("bedrock: LaunchServingVM no members")
	}
	for _, m := range in.Members {
		if m.ModelID == "" {
			return errors.New("bedrock: LaunchServingVM member with empty model id")
		}
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
