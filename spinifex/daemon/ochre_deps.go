package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/nats-io/nats.go/jetstream"
)

// ochreApplianceLaunchTimeout bounds the platform Postgres appliance's whole
// create-and-poll-to-available launch: generous enough for a cold RDS VM
// boot plus initdb on the smallest instance class.
const ochreApplianceLaunchTimeout = 10 * time.Minute

// startOchreVector wires the Ochre vector store's VectorService when
// config.OchreVectorConfig.Enabled is set. Disabled (the default) leaves
// d.ochreVectorService nil, so subscribeAll registers no ochre.vector.*
// subject and daemon behavior is byte-for-byte unchanged. Any failure below
// — JetStream, the master key, or the platform appliance itself — is logged
// and leaves d.ochreVectorService nil rather than failing startCluster: the
// vector store is a feature dependency, never a daemon-boot one.
func (d *Daemon) startOchreVector() {
	cfg := d.config.OchreVector
	if !cfg.Enabled {
		return
	}

	js, err := jetstream.New(d.natsConn)
	if err != nil {
		slog.Warn("Ochre vector store disabled: JetStream unavailable", "err", err)
		return
	}

	masterKey, err := handlers_iam.LoadMasterKey(filepath.Join(filepath.Dir(d.configPath), "master.key"))
	if err != nil {
		slog.Warn("Ochre vector store disabled: master key unavailable", "err", err)
		return
	}

	launcher := handlers_ochrevector.NewRDSApplianceLauncher(d.natsConn, ochreApplianceLaunchTimeout)
	appliance, err := handlers_ochrevector.NewAppliance(js, masterKey, launcher)
	if err != nil {
		slog.Warn("Ochre vector store disabled: appliance construction failed", "err", err)
		return
	}

	ensureCtx, cancel := context.WithTimeout(d.ctx, ochreApplianceLaunchTimeout)
	defer cancel()
	if _, err := appliance.Ensure(ensureCtx); err != nil {
		slog.Warn("Ochre vector store disabled: platform appliance not available", "err", err)
		return
	}

	backend, err := appliance.Connect(d.ctx)
	if err != nil {
		slog.Warn("Ochre vector store disabled: connect to platform appliance failed", "err", err)
		return
	}

	embedModel := cfg.EmbeddingModel
	if embedModel == "" {
		embedModel = gateway_bedrock.DefaultEmbeddingModel
	}
	embedder := gateway_bedrock.NewEmbedder(gateway_bedrock.NewStaticEndpointResolver(map[string]string{
		embedModel: cfg.EmbeddingsEndpoint,
	}))

	store := objectstore.NewS3ObjectStoreFromConfig(admin.DialTarget(d.config.Predastore.Host),
		d.config.Predastore.Region, d.config.Predastore.AccessKey, d.config.Predastore.SecretKey)

	registry := handlers_ochrevector.NewRegistry(js)
	jobs := handlers_ochrevector.NewJobStore(js)
	service := handlers_ochrevector.NewService(registry, backend)
	ingest := handlers_ochrevector.NewIngestService(jobs, registry, backend, store, embedder)

	d.ochreVectorService = handlers_ochrevector.NewVectorService(service, ingest, jobs, registry, backend, embedder)
	slog.Info("Ochre vector store enabled")
}
