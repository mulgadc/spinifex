package daemon

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mulgadc/bluebottle/pkg/masterkey"
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
	masterKey, err := masterkey.ReadShared(filepath.Join(filepath.Dir(d.configPath), "master.key"))
	if err != nil || masterKey == nil {
		slog.Warn("ECS: master key read failed; capacity provisioning disabled until the master key is provisioned",
			"err", err)
		return deps
	}

	clusterSize := 1
	if d.clusterConfig != nil {
		clusterSize = len(d.clusterConfig.Nodes)
	}
	// Retried, because the constructor runs a KV migration that needs JetStream
	// to have responders. A daemon that started first got one attempt, failed
	// it, and had capacity provisioning off for the life of the process with no
	// symptom but a 500 from ProvisionCapacity.
	iamSvc, iamErr := initServiceWithRetry("ECS IAM service", func() (*handlers_iam.IAMServiceImpl, error) {
		return handlers_iam.NewIAMServiceImpl(d.ctx, d.natsConn, masterKey, clusterSize)
	})
	if iamErr != nil {
		// Off rather than fatal: ECS CRUD, EKS and EC2 do not need this, and a
		// node that serves them is worth more than one that exits.
		slog.Warn("ECS: IAM service unavailable; capacity provisioning disabled for this process",
			"err", iamErr)
		return deps
	}
	deps.IAM = iamSvc

	return deps
}
