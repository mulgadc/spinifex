package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	handlers_systemvpc "github.com/mulgadc/spinifex/spinifex/handlers/systemvpc"
)

// A launch can only arrive over the gateway, long after service init, so unlike
// the EKS/ECS IAM deps none of this is built lazily.
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

// The control-plane deps on top of the launch primitives: where the endpoint
// ENI is placed, the instance role the agent authenticates with, the base zone
// the endpoint record lands in, and the VM-state lookup the reconciler needs.
func (d *Daemon) buildRDSDeps() (handlers_rds.Deps, error) {
	gatewayCA := ""
	if d.config.NATS.CACert != "" {
		if caBytes, err := os.ReadFile(d.config.NATS.CACert); err == nil {
			gatewayCA = string(caBytes)
		} else {
			slog.Warn("RDS: read gateway CACert failed; the DB VM agent will not verify the gateway over TLS",
				"path", d.config.NATS.CACert, "err", err)
		}
	}

	// No degraded mode: a create cannot stage a master password without it, and
	// the fetch that replays one is answered by any node in the queue group.
	masterKey, err := handlers_iam.LoadMasterKey(filepath.Join(filepath.Dir(d.configPath), "master.key"))
	if err != nil {
		return handlers_rds.Deps{}, fmt.Errorf("load RDS master key: %w", err)
	}

	return handlers_rds.Deps{
		CACertPath:    d.config.NATS.CACert,
		CAKeyPath:     clusterCAKeyPath(d.config.NATS.CACert),
		MasterKey:     masterKey,
		Launch:        d.buildRDSLaunchDeps(),
		Network:       d.vpcService,
		IAM:           d.systemRoleEnsurer,
		InstanceState: handlers_rds.NewDescribeInstanceState(d.describeInstancesFanOut),
		Instances:     handlers_rds.NewNATSInstanceCommander(d.natsConn),
		Snapshots:     d.snapshotService,
		Storage:       d.volumeService,
		BaseDomain:    d.dnsBaseDomain,
		HolderID:      d.node,
		// A DB VM reaches the daemon only over the mgmt bridge, the same
		// constraint the EKS control-plane VM has.
		GatewayURL:       d.resolveSystemGatewayBaseURL(),
		GatewayCACert:    gatewayCA,
		BootstrapTimeout: time.Duration(d.config.RDS.BootstrapTimeoutSeconds) * time.Second,
		FailureGrace:     time.Duration(d.config.RDS.FailureGraceSeconds) * time.Second,
		Backup: handlers_rds.BackupPolicy{
			RetentionCapDays:       int64(d.config.RDS.BackupRetentionCapDays),
			RetentionDays:          int64(d.config.RDS.BackupRetentionDays),
			BackupWindowBlock:      d.config.RDS.BackupWindowBlock,
			MaintenanceWindowBlock: d.config.RDS.MaintenanceWindowBlock,
			SweepDeleteLimit:       d.config.RDS.BackupSweepDeleteLimit,
		},
	}, nil
}

// Fans out across every host so a DB VM is observed wherever it landed; a
// node-local describe would report an instance on another node as missing.
func (d *Daemon) describeInstancesFanOut(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error) {
	expected := 1
	if d.clusterConfig != nil && len(d.clusterConfig.Nodes) > 0 {
		expected = len(d.clusterConfig.Nodes)
	}
	return gateway_ec2_instance.DescribeInstances(context.Background(), input, d.natsConn, expected, accountID)
}
