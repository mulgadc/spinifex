package daemon

import (
	"log/slog"

	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	handlers_bedrock "github.com/mulgadc/spinifex/spinifex/handlers/bedrock"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/nats-io/nats.go/jetstream"
)

// buildBedrockLaunchDeps assembles the launch-time collaborators: VPC/volume
// plumbing mirrors buildRDSLaunchDeps exactly (same underlying EC2 handler
// set), plus the weights snapshot resolver the RDS launcher has no equivalent
// of.
func (d *Daemon) buildBedrockLaunchDeps() handlers_bedrock.LaunchDeps {
	js, err := jetstream.New(d.natsConn)
	var weights = gateway_bedrock.NoopWeightsResolver
	if err != nil {
		slog.Warn("Bedrock: jetstream init failed; serving VMs cannot resolve staged weights", "err", err)
	} else {
		clusterSize := 1
		if d.clusterConfig != nil {
			clusterSize = len(d.clusterConfig.Nodes)
		}
		weights = gateway_bedrock.NewWeightsStore(js, clusterSize)
	}

	return handlers_bedrock.LaunchDeps{
		Config: d.config,
		SystemVPC: handlers_systemvpc.Deps{
			VPC:      d.vpcService,
			SG:       d.vpcService,
			IGW:      d.igwService,
			RT:       d.routeTableService,
			NGW:      d.natGatewayService,
			EIP:      d.eipService,
			NATSConn: d.natsConn,
		},
		VPC:      d.vpcService,
		Instance: d,
		Image:    d.imageService,
		Volume:   d.volumeService,
		Attacher: handlers_rds.NewNATSVolumeAttacher(d.natsConn),
		Weights:  weights,
		// A fresh plumber rather than d.networkPlumber: it is stateless, and
		// taking it here would order this call after startLocal.
		HostPort: host.NewOVSPlumber(),
		NodeID:   d.node,
	}
}

// buildBedrockServiceDeps assembles the ServiceDeps the Bedrock endpoint
// lifecycle Service needs. GPU is left at its zero value (a true nil
// interface) when no GPU manager is present: assigning a typed nil
// *daemonGPUSnapshotter instead would produce a non-nil interface wrapping a
// nil pointer, defeating admitCapacity's own "snapshotter == nil" check.
func (d *Daemon) buildBedrockServiceDeps() handlers_bedrock.ServiceDeps {
	clusterSize := 1
	if d.clusterConfig != nil {
		clusterSize = len(d.clusterConfig.Nodes)
	}
	deps := handlers_bedrock.ServiceDeps{
		Config:   d.config,
		Launch:   d.buildBedrockLaunchDeps(),
		NodeID:   d.node,
		Replicas: clusterSize,
	}
	if d.gpuManager != nil {
		deps.GPU = &daemonGPUSnapshotter{d: d}
	}
	return deps
}

// daemonGPUSnapshotter adapts *gpu.Manager to handlers_bedrock's Snapshot-only
// capacity-check surface, keeping the gpu package out of that handler package
// the same way daemonGPUClaimer keeps it out of handlers_ec2_instance.
//
// It holds the daemon rather than the manager because applyGPUConfig swaps
// d.gpuManager on a passthrough toggle. Capturing the pointer here would leave
// this reading an orphaned pool that sees no claims and so admits everything.
type daemonGPUSnapshotter struct {
	d *Daemon
}

func (g *daemonGPUSnapshotter) Snapshot() []gpu.PoolEntry {
	g.d.mu.Lock()
	mgr := g.d.gpuManager
	g.d.mu.Unlock()
	if mgr == nil {
		return nil
	}
	return mgr.Snapshot()
}
