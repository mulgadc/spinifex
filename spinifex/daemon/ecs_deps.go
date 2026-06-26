package daemon

import (
	"log/slog"
	"os"
	"path/filepath"

	handlers_ecs "github.com/mulgadc/spinifex/spinifex/handlers/ecs"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// buildECSServiceDeps assembles the ECS service Deps for ProvisionCapacity: the
// gateway endpoint/CA seeded into container-instance user-data, the image
// resolver, the customer RunInstances path, and (when the master key loads) a
// KV-backed IAM service for the ecsInstanceRole instance profile.
func (d *Daemon) buildECSServiceDeps() handlers_ecs.Deps {
	gatewayCA := ""
	if d.config.NATS.CACert != "" {
		if caBytes, readErr := os.ReadFile(d.config.NATS.CACert); readErr == nil {
			gatewayCA = string(caBytes)
		} else {
			slog.Warn("ECS: read gateway CACert failed; container instances will not verify the gateway over TLS",
				"path", d.config.NATS.CACert, "err", readErr)
		}
	}

	deps := handlers_ecs.Deps{
		GatewayBaseURL: d.resolveGatewayBaseURL(),
		GatewayCACert:  gatewayCA,
		Images:         d.imageService,
		RunInstances:   d.RunWorkerInstance,
	}

	// A KV-backed IAM service (sharing the gateway's buckets over NATS) lets ECS
	// find-or-create the ecsInstanceRole instance profile container instances
	// expose over IMDS. Only wired when the master key is present; without it
	// capacity provisioning is disabled.
	masterKey, err := handlers_iam.LoadMasterKey(filepath.Join(filepath.Dir(d.configPath), "master.key"))
	if err != nil || masterKey == nil {
		slog.Warn("ECS: LoadMasterKey failed; capacity provisioning disabled until the master key is provisioned",
			"err", err)
		return deps
	}

	clusterSize := 1
	if d.clusterConfig != nil {
		clusterSize = len(d.clusterConfig.Nodes)
	}
	if iamSvc, iamErr := handlers_iam.NewIAMServiceImpl(d.natsConn, masterKey, clusterSize); iamErr != nil {
		slog.Warn("ECS: IAM service init failed; capacity provisioning disabled", "err", iamErr)
	} else {
		deps.IAM = iamSvc
	}

	return deps
}
