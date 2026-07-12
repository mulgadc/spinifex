package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"

	"github.com/mulgadc/spinifex/cmd/ecs-agent/credentials"
	"github.com/mulgadc/spinifex/internal/ecsgw"
	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
)

// controlPlane is the agent's channel to the AWS gateway. It replaces the direct
// NATS publisher: every method is a SigV4-signed gateway request, so the NATS bus
// stays host-internal. Tests inject a fake; production uses gatewayControlPlane.
type controlPlane interface {
	// Register (re-)registers the container instance. Called at boot and on every
	// heartbeat tick — the gateway's RegisterContainerInstance is idempotent and
	// refreshes LastSeen, folding liveness into registration (no bus.Heartbeat).
	Register(id identity) error
	// SubmitTaskState reports a task's RUNNING/STOPPED transition.
	SubmitTaskState(st bus.TaskState) error
	// PollAssignments drains this instance's assignment + stop inboxes, acking the
	// assign taskIDs and stop taskIDs accepted on the previous poll. Returns the
	// still-pending assignments and stop directives.
	PollAssignments(cluster, instance string, ackAssigns, ackStops []string) ([]bus.Assign, []bus.StopDirective, error)
	// ReportTaskGPU reports the local ledger's pinned device UUIDs for a task's
	// containers, so the control plane can populate DescribeTasks gpuIds.
	ReportTaskGPU(cluster, task string, containers []handlers_ecs.ContainerGPUReport) error
}

// gatewayControlPlane implements controlPlane over the SigV4 ECS gateway client.
type gatewayControlPlane struct {
	client *ecsgw.Client
}

var _ controlPlane = (*gatewayControlPlane)(nil)

// pollTimeout bounds a single PollAssignments request; larger than the register
// timeout to leave room for a future server-side long-poll.
const pollTimeout = 30 * time.Second

// newGatewayControlPlane builds the SigV4 client signing with the instance-role
// credentials from IMDS (fetched per call so rotation is transparent) + pinned CA.
func newGatewayControlPlane(cfg config, creds credentials.CredentialsProvider) (*gatewayControlPlane, error) {
	client, err := ecsgw.New(cfg.GatewayURL, cfg.GatewayCA, credentialsFunc(creds), cfg.Region, pollTimeout)
	if err != nil {
		return nil, err
	}
	return &gatewayControlPlane{client: client}, nil
}

// credentialsFunc adapts the agent's IMDS CredentialsProvider to the ecsgw
// per-call credentials hook.
func credentialsFunc(p credentials.CredentialsProvider) ecsgw.CredentialsFunc {
	return func(ctx context.Context) (string, string, string, error) {
		c, err := p.Retrieve(ctx)
		if err != nil {
			return "", "", "", err
		}
		return c.AccessKeyID, c.SecretAccessKey, c.SessionToken, nil
	}
}

func (g *gatewayControlPlane) Register(id identity) error {
	in := &ecs.RegisterContainerInstanceInput{
		Cluster:                  aws.String(id.ClusterName),
		InstanceIdentityDocument: aws.String(id.InstanceID),
		TotalResources: []*ecs.Resource{
			{Name: aws.String("CPU"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(int64(id.Capacity.CPU))},
			{Name: aws.String("MEMORY"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(int64(id.Capacity.MemoryMiB))},
		},
		VersionInfo: &ecs.VersionInfo{AgentVersion: aws.String(id.AgentVersion)},
	}
	if len(id.Capacity.GPUIDs) > 0 {
		in.TotalResources = append(in.TotalResources, &ecs.Resource{
			Name: aws.String("GPU"), Type: aws.String("STRINGSET"), StringSetValue: aws.StringSlice(id.Capacity.GPUIDs),
		})
	}
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal register: %w", err)
	}
	if _, err := g.client.Call("RegisterContainerInstance", body); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	return nil
}

func (g *gatewayControlPlane) SubmitTaskState(st bus.TaskState) error {
	in := &ecs.SubmitTaskStateChangeInput{
		Cluster: aws.String(st.ClusterName),
		Task:    aws.String(st.TaskID),
		Status:  aws.String(st.LastStatus),
		Reason:  aws.String(st.Reason),
	}
	for _, c := range st.Containers {
		csc := &ecs.ContainerStateChange{
			ContainerName: aws.String(c.Name),
			Status:        aws.String(c.Status),
			RuntimeId:     aws.String(c.ContainerID),
		}
		if c.ExitCode != nil {
			csc.ExitCode = aws.Int64(int64(*c.ExitCode))
		}
		in.Containers = append(in.Containers, csc)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal task-state: %w", err)
	}
	if _, err := g.client.Call("SubmitTaskStateChange", body); err != nil {
		return fmt.Errorf("submit task-state: %w", err)
	}
	return nil
}

func (g *gatewayControlPlane) PollAssignments(cluster, instance string, ackAssigns, ackStops []string) ([]bus.Assign, []bus.StopDirective, error) {
	in := &handlers_ecs.PollAssignmentsInput{
		Cluster:           cluster,
		ContainerInstance: instance,
		AckTaskIDs:        ackAssigns,
		AckStopIDs:        ackStops,
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal poll: %w", err)
	}
	resp, err := g.client.Call("PollAssignments", body)
	if err != nil {
		return nil, nil, fmt.Errorf("poll: %w", err)
	}
	var out handlers_ecs.PollAssignmentsOutput
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, nil, fmt.Errorf("decode poll: %w", err)
	}
	return out.Assignments, out.Stops, nil
}

func (g *gatewayControlPlane) ReportTaskGPU(cluster, task string, containers []handlers_ecs.ContainerGPUReport) error {
	in := &handlers_ecs.ReportTaskGPUInput{Cluster: cluster, Task: task, Containers: containers}
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal report-task-gpu: %w", err)
	}
	if _, err := g.client.Call("ReportTaskGPU", body); err != nil {
		return fmt.Errorf("report task gpu: %w", err)
	}
	return nil
}
