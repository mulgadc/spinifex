package handlers_ecs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// eniRequestTimeout bounds each ENI control-plane NATS round-trip. Attach drives
// the QMP hot-plug pipeline so it gets the same 30s budget the gateway uses.
const eniRequestTimeout = 30 * time.Second

// taskENIDeviceIndex is the secondary device index every awsvpc task ENI hot-plugs
// onto; the primary NIC (index 0) is the instance's own ENI.
const taskENIDeviceIndex = 1

// eniAllocation is the identity of a freshly created task ENI.
type eniAllocation struct {
	ENIID        string
	MacAddress   string
	PrivateIP    string
	SubnetID     string
	AttachmentID string
}

// eniController is the scheduler's ENI control-plane: it owns create, attach,
// detach, and delete for awsvpc task ENIs (single writer). The agent never calls
// these — it only wires the guest netns once the device is hot-plugged.
type eniController interface {
	Allocate(ctx context.Context, accountID, subnetID string, securityGroups []*string) (eniAllocation, error)
	Attach(ctx context.Context, accountID, instanceID, eniID string) (attachmentID string, err error)
	Release(ctx context.Context, accountID string, rec *TaskRecord) error
}

// natsENIController drives the EC2 ENI handlers over NATS. Create/Delete are plain
// request/response; Attach/Detach dispatch to the owning daemon via ec2.cmd.{id}
// (the Sprint 3 hot-plug path).
type natsENIController struct {
	nc      *nats.Conn
	timeout time.Duration
}

var _ eniController = (*natsENIController)(nil)

func newNATSENIController(nc *nats.Conn) *natsENIController {
	return &natsENIController{nc: nc, timeout: eniRequestTimeout}
}

// Allocate creates an ENI in subnetID with the given security groups.
func (c *natsENIController) Allocate(ctx context.Context, accountID, subnetID string, securityGroups []*string) (eniAllocation, error) {
	in := &ec2.CreateNetworkInterfaceInput{
		SubnetId:    aws.String(subnetID),
		Groups:      securityGroups,
		Description: aws.String("ecs-awsvpc-task"),
	}
	out, err := utils.NatsRequest[ec2.CreateNetworkInterfaceOutput](ctx, c.nc, "ec2.CreateNetworkInterface", in, c.timeout, accountID)
	if err != nil {
		return eniAllocation{}, fmt.Errorf("create task ENI: %w", err)
	}
	if out.NetworkInterface == nil || aws.StringValue(out.NetworkInterface.NetworkInterfaceId) == "" {
		return eniAllocation{}, fmt.Errorf("create task ENI: empty response")
	}
	ni := out.NetworkInterface
	return eniAllocation{
		ENIID:      aws.StringValue(ni.NetworkInterfaceId),
		MacAddress: strings.ToLower(aws.StringValue(ni.MacAddress)),
		PrivateIP:  aws.StringValue(ni.PrivateIpAddress),
		SubnetID:   subnetID,
	}, nil
}

// Attach hot-plugs eniID onto instanceID and returns the attachment ID.
func (c *natsENIController) Attach(ctx context.Context, accountID, instanceID, eniID string) (string, error) {
	cmd := types.EC2InstanceCommand{
		ID:         instanceID,
		Attributes: types.EC2CommandAttributes{AttachENI: true},
		AttachENIData: &types.AttachENIData{
			NetworkInterfaceID: eniID,
			DeviceIndex:        taskENIDeviceIndex,
		},
	}
	out, err := utils.NatsRequest[ec2.AttachNetworkInterfaceOutput](ctx, c.nc, eniCmdSubject(instanceID), cmd, c.timeout, accountID)
	if err != nil {
		return "", fmt.Errorf("attach task ENI %s -> %s: %w", eniID, instanceID, err)
	}
	return aws.StringValue(out.AttachmentId), nil
}

// Release detaches (force) then deletes the task ENI. Both steps treat a NotFound
// as success so the graceful-stop and reaper paths can each run idempotently.
func (c *natsENIController) Release(ctx context.Context, accountID string, rec *TaskRecord) error {
	if rec == nil || rec.ENIID == "" {
		return nil
	}
	if rec.ENIAttachmentID != "" && rec.ContainerInstanceID != "" {
		cmd := types.EC2InstanceCommand{
			ID:         rec.ContainerInstanceID,
			Attributes: types.EC2CommandAttributes{DetachENI: true},
			DetachENIData: &types.DetachENIData{
				AttachmentID: rec.ENIAttachmentID,
				Force:        true,
			},
		}
		_, err := utils.NatsRequest[ec2.DetachNetworkInterfaceOutput](ctx, c.nc, eniCmdSubject(rec.ContainerInstanceID), cmd, c.timeout, accountID)
		if err != nil && !isENINotFound(err) {
			return fmt.Errorf("detach task ENI %s: %w", rec.ENIID, err)
		}
	}

	del := &ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(rec.ENIID)}
	_, err := utils.NatsRequest[ec2.DeleteNetworkInterfaceOutput](ctx, c.nc, "ec2.DeleteNetworkInterface", del, c.timeout, accountID)
	if err != nil && !isENINotFound(err) {
		return fmt.Errorf("delete task ENI %s: %w", rec.ENIID, err)
	}
	return nil
}

func eniCmdSubject(instanceID string) string {
	return fmt.Sprintf("ec2.cmd.%s", instanceID)
}

// reclaimTaskENI releases an awsvpc task's ENI on the single-writer teardown path
// (graceful stop + reaper both reach it). Best effort: a failure is logged and the
// ENI fields stay on the record for a later retry, never blocking the STOPPED
// transition. No-op for non-awsvpc tasks or tasks with no ENI.
func (s *Service) reclaimTaskENI(ctx context.Context, accountID string, task *TaskRecord) {
	if s.eni == nil || task == nil || task.NetworkMode != NetworkModeAwsvpc || task.ENIID == "" {
		return
	}
	if err := s.eni.Release(ctx, accountID, task); err != nil {
		slog.ErrorContext(ctx, "ECS: task ENI release failed",
			"task", task.TaskID, "eni", task.ENIID, "err", err)
	}
}

// isENINotFound reports whether err is an idempotent already-gone signal — a
// missing ENI or attachment means the teardown is already converged.
func isENINotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "InvalidNetworkInterfaceID.NotFound") ||
		strings.Contains(msg, "InvalidAttachmentID.NotFound")
}
