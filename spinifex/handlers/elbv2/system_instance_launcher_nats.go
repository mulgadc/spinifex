package handlers_elbv2

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// natsSystemInstanceLauncher implements SystemInstanceLauncher over NATS request/reply,
// fanning ALB-VM launches across the cluster via the spinifex-workers queue group
// (system.LaunchInstance.{type}); terminate goes directly to the owning daemon.
type natsSystemInstanceLauncher struct {
	nc      *nats.Conn
	timeout time.Duration
}

const defaultSystemInstanceTimeout = 60 * time.Second

// NewNATSSystemInstanceLauncher returns a NATS-based SystemInstanceLauncher.
// Pass timeout=0 to use the 60 s default (sized for direct-boot microVM
// start + EIP allocation on a busy cluster).
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
