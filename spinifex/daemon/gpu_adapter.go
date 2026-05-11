package daemon

import (
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
)

// daemonGPUClaimer adapts the daemon's gpu.Manager + GPUModelOverrides config to
// the transport-neutral handlers_ec2_instance.GPUClaimer surface. Keeps the gpu
// package out of the instance service.
type daemonGPUClaimer struct {
	d *Daemon
}

var _ handlers_ec2_instance.GPUClaimer = (*daemonGPUClaimer)(nil)

func (g *daemonGPUClaimer) Claim(instanceID string) (string, bool, error) {
	dev, err := g.d.gpuManager.Claim(instanceID)
	if err != nil {
		return "", false, err
	}
	return dev.PCIAddress, gpuXVGAEnabled(dev, g.d.config.Daemon.GPUModelOverrides), nil
}

func (g *daemonGPUClaimer) Release(instanceID string) error {
	return g.d.gpuManager.Release(instanceID)
}
