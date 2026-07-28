package daemon

import (
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
)

// buildRDSLaunchDeps assembles the collaborators a DB VM is composed from: the
// EC2 VPC-family services the shared RDS system VPC is built out of, the ENI
// surface both of the VM's NICs are created through, the system-instance
// launcher, the AMI resolver, and the data volume's create + hot-plug attach.
//
// All of them are already initialised by the time an RDS launch can run — the
// first one arrives over the gateway, long after service init — so unlike the
// EKS/ECS IAM deps none of this is built lazily.
func (d *Daemon) buildRDSLaunchDeps() handlers_rds.LaunchDeps {
	return handlers_rds.LaunchDeps{
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
	}
}
