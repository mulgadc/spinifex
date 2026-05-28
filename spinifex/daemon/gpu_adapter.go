package daemon

import (
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

func (g *daemonGPUClaimer) Claim(instanceID, profileName string) (*gpu.GPUAttachment, error) {
	dev, mig, err := g.d.gpuManager.Claim(instanceID, profileName)
	if err != nil {
		return nil, err
	}
	if mig != nil {
		return &gpu.GPUAttachment{MdevPath: mig.MdevPath}, nil
	}
	return &gpu.GPUAttachment{
		PCIAddress:  dev.PCIAddress,
		XVGAEnabled: gpuXVGAEnabled(dev, g.d.config.Daemon.GPUModelOverrides),
	}, nil
}

func (g *daemonGPUClaimer) Release(instanceID string) error {
	return g.d.gpuManager.Release(instanceID)
}
