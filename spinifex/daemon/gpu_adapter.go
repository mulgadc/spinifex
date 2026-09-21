package daemon

import (
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/gpu"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
)

// daemonGPUClaimer adapts the daemon's gpu.Manager + GPUModelOverrides config to
// the transport-neutral handlers_ec2_instance.GPUClaimer surface. Keeps the gpu
// package out of the instance service.
type daemonGPUClaimer struct {
	d *Daemon
}

var _ handlers_ec2_instance.GPUClaimer = (*daemonGPUClaimer)(nil)

func (g *daemonGPUClaimer) manager() *gpu.Manager {
	g.d.mu.Lock()
	defer g.d.mu.Unlock()
	return g.d.gpuManager
}

func (g *daemonGPUClaimer) Claim(instanceID, profileName string, count int) ([]gpu.GPUAttachment, error) {
	if count < 1 {
		return nil, fmt.Errorf("GPU claim count must be positive: %d", count)
	}
	mgr := g.manager()
	if mgr == nil {
		return nil, errors.New("GPU manager is unavailable")
	}

	attachments := make([]gpu.GPUAttachment, 0, count)
	for range count {
		dev, mig, err := mgr.Claim(instanceID, profileName)
		if err != nil {
			if rollbackErr := mgr.Release(instanceID); rollbackErr != nil &&
				!errors.Is(rollbackErr, gpu.ErrNoGPUClaimed) {
				return nil, errors.Join(err, fmt.Errorf("roll back GPU claims: %w", rollbackErr))
			}
			return nil, err
		}
		if mig != nil {
			attachments = append(attachments, gpu.GPUAttachment{MdevPath: mig.MdevPath})
			continue
		}
		attachments = append(attachments, gpu.GPUAttachment{
			PCIAddress:  dev.PCIAddress,
			XVGAEnabled: gpuXVGAEnabled(dev, g.d.config.Daemon.GPUModelOverrides),
		})
	}
	return attachments, nil
}

func (g *daemonGPUClaimer) Release(instanceID string) error {
	mgr := g.manager()
	if mgr == nil {
		return gpu.ErrNoGPUClaimed
	}
	return mgr.Release(instanceID)
}
