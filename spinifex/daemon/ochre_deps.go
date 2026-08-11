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

	vectorService := handlers_ochrevector.NewVectorService(service, ingest, jobs, registry, backend, embedder)

	// A shutdown that lands in the gap between Connect succeeding above and
	// this check does not leave anything to unwind: nothing has been
	// registered yet, so there are no subjects or map entries to clean up --
	// this is a best-effort skip of a subscribe attempt that would very
	// likely fail on a closing NATS connection anyway, not a correctness
	// requirement.
	if d.ctx.Err() != nil {
		slog.Warn("Ochre vector store: daemon shutting down before appliance came up; not registering subjects")
		return
	}

	// Registered here rather than in subscribeAll: these six subjects only
	// exist once the appliance above is actually connected, which can be
	// minutes after subscribeAll already ran. registerNatsSubs is the same
	// table-driven mechanism subscribeAll itself uses, so a queue-group
	// registration here is indistinguishable from one made at boot.
	subs := []natsSub{
		{handlers_ochrevector.SubjectCreateIndex, handleNATSRequest(vectorService.CreateIndex), "spinifex-workers"},
		{handlers_ochrevector.SubjectDeleteIndex, handleNATSRequest(vectorService.DeleteIndex), "spinifex-workers"},
		{handlers_ochrevector.SubjectListIndexes, handleNATSRequest(vectorService.ListIndexes), "spinifex-workers"},
		{handlers_ochrevector.SubjectIngest, handleNATSRequest(vectorService.Ingest), "spinifex-workers"},
		{handlers_ochrevector.SubjectDescribeJob, handleNATSRequest(vectorService.DescribeJob), "spinifex-workers"},
		{handlers_ochrevector.SubjectQuery, handleNATSRequest(vectorService.Query), "spinifex-workers"},
	}
	if err := d.registerNatsSubs(subs); err != nil {
		slog.Error("Ochre vector store: failed to register NATS subjects", "err", err)
		return
	}

	// Assigned last, and only after every subject above is live: a reader
	// (there is none today beyond observability) must never see a non-nil
	// service whose subjects are not yet actually serving.
	d.ochreVectorService = vectorService
	slog.Info("Ochre vector store enabled")
}
