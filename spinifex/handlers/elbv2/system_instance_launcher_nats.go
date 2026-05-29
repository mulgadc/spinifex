package handlers_elbv2

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// natsSystemInstanceLauncher implements SystemInstanceLauncher by
// dispatching LaunchSystemInstance / TerminateSystemInstance over NATS so
// ALB-VMs fan out across the cluster instead of piling on the daemon node
// that handled the upstream CreateLoadBalancer call.
//
// Subjects:
//   - system.LaunchInstance.{type}     (queue group spinifex-workers)
//   - system.TerminateInstance.{id}    (single subscriber: the owning daemon)
//
// The daemons subscribe with capacity-aware logic in
// ResourceManager.updateInstanceSubscriptions; only nodes with free
// vCPU/memory for the requested type carry the launch subscription, so
// over-full nodes naturally drop out of the queue group.
type natsSystemInstanceLauncher struct {
	nc      *nats.Conn
	timeout time.Duration
}

const defaultSystemInstanceTimeout = 60 * time.Second

// NewNATSSystemInstanceLauncher returns a SystemInstanceLauncher that
// publishes requests over NATS request/reply. timeout caps each round trip;
// pass 0 to use the default (60s, sized for direct-boot microVM start +
// EIP allocation latency on a busy cluster).
func NewNATSSystemInstanceLauncher(nc *nats.Conn, timeout time.Duration) SystemInstanceLauncher {
	if timeout <= 0 {
		timeout = defaultSystemInstanceTimeout
	}
	return &natsSystemInstanceLauncher{nc: nc, timeout: timeout}
}

// natsSystemInstanceLaunchEnvelope mirrors the daemon-side wire format so
// ELBv2 and the daemon agree on JSON shape without sharing a package.
type natsSystemInstanceLaunchEnvelope struct {
	Output *SystemInstanceOutput `json:"output,omitempty"`
	Error  string                `json:"error,omitempty"`
}

type natsSystemInstanceTerminateEnvelope struct {
	Error string `json:"error,omitempty"`
}

func (l *natsSystemInstanceLauncher) LaunchSystemInstance(input *SystemInstanceInput) (*SystemInstanceOutput, error) {
	if input == nil {
		return nil, fmt.Errorf("system instance input is nil")
	}
	if input.InstanceType == "" {
		return nil, fmt.Errorf("system instance input missing InstanceType")
	}

	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal SystemInstanceInput: %w", err)
	}

	subject := fmt.Sprintf("system.LaunchInstance.%s", input.InstanceType)
	reply, err := l.nc.Request(subject, payload, l.timeout)
	if err != nil {
		return nil, fmt.Errorf("nats request %s: %w", subject, err)
	}

	var env natsSystemInstanceLaunchEnvelope
	if err := json.Unmarshal(reply.Data, &env); err != nil {
		return nil, fmt.Errorf("decode launch reply: %w", err)
	}
	if env.Error != "" {
		return nil, fmt.Errorf("%s", env.Error)
	}
	if env.Output == nil {
		return nil, fmt.Errorf("launch reply missing output payload")
	}
	return env.Output, nil
}

func (l *natsSystemInstanceLauncher) TerminateSystemInstance(instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("system instance terminate: empty instance ID")
	}

	subject := fmt.Sprintf("system.TerminateInstance.%s", instanceID)
	reply, err := l.nc.Request(subject, nil, l.timeout)
	if err != nil {
		return fmt.Errorf("nats request %s: %w", subject, err)
	}

	var env natsSystemInstanceTerminateEnvelope
	if err := json.Unmarshal(reply.Data, &env); err != nil {
		return fmt.Errorf("decode terminate reply: %w", err)
	}
	if env.Error != "" {
		return fmt.Errorf("%s", env.Error)
	}
	return nil
}

var _ SystemInstanceLauncher = (*natsSystemInstanceLauncher)(nil)
