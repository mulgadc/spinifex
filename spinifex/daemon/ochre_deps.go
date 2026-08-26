package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	handlers_bedrock "github.com/mulgadc/spinifex/spinifex/handlers/bedrock"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/nats-io/nats.go/jetstream"
)

// ochreApplianceLaunchTimeout bounds the platform Postgres appliance's whole
// create-and-poll-to-available launch: generous enough for a cold RDS VM
// boot plus initdb on the smallest instance class.
const ochreApplianceLaunchTimeout = 10 * time.Minute

// ochreStartupLogAttempts and ochreStartupLogEvery throttle the connect-retry
// log: warn on the first few attempts, then only every Nth, so a persistently
// unreachable appliance leaves a periodic breadcrumb without flooding the log.
const (
	ochreStartupLogAttempts = 3
	ochreStartupLogEvery    = 20
)

// ochreStartupInitialBackoff and ochreStartupMaxBackoff bound the wait
// between attempts: it doubles from the initial value and is capped so a
// run of early failures cannot stall an eventually-successful attempt for
// an unbounded amount of time.
const (
	ochreStartupInitialBackoff = 15 * time.Second
	ochreStartupMaxBackoff     = 3 * time.Minute
)

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
	// Give the daemon a routed presence in the appliance's system-VPC subnet
	// before Connect dials it: a tag-filtered ENI describe resolves the real
	// dial IP and subnet, never the unroutable vanity endpoint hostname.
	appliance.WithHostPort(handlers_ochrevector.HostPortDeps{
		VPC:      d.vpcService,
		HostPort: host.NewOVSPlumber(),
		NodeID:   d.node,
	})

	// registry, jobs, kb and ds need only js, never a live backend, so they
	// are built (and attached) here rather than after Connect: Teardown must
	// be able to purge them even for an appliance that never comes up.
	// registry/jobs are reused below, once connected, rather than
	// reconstructed; kb/ds are gateway-owned metadata this daemon never reads
	// or writes itself -- they exist here solely so Teardown can purge them
	// alongside registry/jobs.
	registry := handlers_ochrevector.NewRegistry(js)
	jobs := handlers_ochrevector.NewJobStore(js)
	appliance.WithStores(registry, jobs)
	appliance.WithKBStores(handlers_ochrevector.NewKBStore(js), handlers_ochrevector.NewDataSourceStore(js))

	// Publish the appliance and register the operator teardown subject BEFORE
	// Connect: recovering a broken singleton is teardown's whole purpose, so it
	// must stay reachable even when the appliance is unreachable and the connect
	// loop below fails. Teardown needs only js+launcher+KV, never a live backend.
	d.mu.Lock()
	d.ochreAppliance = appliance
	d.mu.Unlock()
	if err := d.registerNatsSubs([]natsSub{
		{handlers_ochrevector.SubjectTeardownAppliance, handleNATSRequest(d.handleOchreApplianceTeardown), "spinifex-workers"},
	}); err != nil {
		slog.Error("Ochre vector store: failed to register appliance teardown subject", "err", err)
	}

	backend, err := d.connectOchreAppliance(appliance)
	if err != nil {
		// connectOchreAppliance only returns on daemon shutdown now; there is
		// no give-up path, so an error here is teardown, not a failure to wire.
		slog.Info("Ochre vector store: appliance connect abandoned on shutdown", "err", err)
		return
	}

	// Embedder/reranker are bundle members: DynamicEndpointResolver resolves
	// each model id to the co-resident bundle VM's own port, in-process
	// against d.bedrockService rather than a NATS round trip to itself.
	endpointResolver := handlers_bedrock.NewDynamicEndpointResolver(d.bedrockService, nil, 0)
	embedder := gateway_bedrock.NewEmbedder(endpointResolver)

	// RerankModel unset leaves reranker nil: Query degrades to plain KNN
	// rather than resolving a default rerank model nobody asked for.
	var reranker handlers_ochrevector.Reranker
	if cfg.RerankModel != "" {
		reranker = gateway_bedrock.NewReranker(endpointResolver, cfg.RerankModel)
	}

	store := objectstore.NewS3ObjectStoreFromConfig(admin.DialTarget(d.config.Predastore.Host),
		d.config.Predastore.Region, d.config.Predastore.AccessKey, d.config.Predastore.SecretKey)

	// registry and jobs were already built above (and attached to appliance
	// via WithStores) before Connect was attempted; reused here, not rebuilt.
	service := handlers_ochrevector.NewService(registry, backend)
	ingest := handlers_ochrevector.NewIngestService(jobs, registry, backend, store, embedder)

	vectorService := handlers_ochrevector.NewVectorService(service, ingest, jobs, registry, backend, embedder, reranker)

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

	// Registered here rather than in subscribeAll: these subjects only
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
		{handlers_ochrevector.SubjectListJobs, handleNATSRequest(vectorService.ListJobs), "spinifex-workers"},
		{handlers_ochrevector.SubjectStopJob, handleNATSRequest(vectorService.StopJob), "spinifex-workers"},
	}

	// backup/restore need RegrantAccount alongside EnsureAccount, which is
	// not part of VectorBackend itself; every real backend (pgxBackend)
	// implements it, so this only ever skips registration for a future
	// VectorBackend that does not.
	if granter, ok := backend.(handlers_ochrevector.AccountGranter); ok {
		backupSvc := handlers_ochrevector.NewBackupService(appliance, granter, store, handlers_ochrevector.ExecPgDumper{})
		d.mu.Lock()
		d.ochreBackupService = backupSvc
		d.mu.Unlock()
		subs = append(subs,
			natsSub{handlers_ochrevector.SubjectBackupAccount, handleNATSRequest(backupSvc.Backup), "spinifex-workers"},
			natsSub{handlers_ochrevector.SubjectRestoreAccount, handleNATSRequest(backupSvc.Restore), "spinifex-workers"},
		)
	} else {
		slog.Warn("Ochre vector store: backend does not support account backup/restore; subjects not registered")
	}

	if err := d.registerNatsSubs(subs); err != nil {
		slog.Error("Ochre vector store: failed to register NATS subjects", "err", err)
		return
	}

	// Assigned last, and only after every subject above is live: a reader
	// (there is none today beyond observability) must never see a non-nil
	// service whose subjects are not yet actually serving.
	d.ochreVectorService = vectorService

	// Reconcile before the scheduler starts sweeping, so a job this pass
	// enqueues for a rebuilt index is already PENDING for the scheduler's
	// first Sweep rather than waiting for its next tick.
	d.reconcileOchreIndexes(d.ctx, registry, backend, ingest)

	go d.runOchreIngestScheduler(ingest)
	slog.Info("Ochre vector store enabled")
}

// reconcileOchreIndexes re-creates and re-ingests every registry index whose
// backing Postgres table is missing -- the state left by a teardown that
// preserved metadata (registry/KB/DataSource) but destroyed the RDS
// instance, once Ensure has connected to a freshly-provisioned replacement.
// An index whose table survived (a plain restart) is left alone: no
// EnsureAccount/CreateIndex call and no ingest job for it. Best-effort: a
// failure on one record is logged and the pass continues to the rest rather
// than failing the appliance connect that already succeeded.
func (d *Daemon) reconcileOchreIndexes(ctx context.Context, registry *handlers_ochrevector.Registry, backend handlers_ochrevector.VectorBackend, ingest *handlers_ochrevector.IngestService) {
	recs, err := registry.ListAll(ctx)
	if err != nil {
		slog.Warn("Ochre vector store: reconcile indexes: list registry failed", "err", err)
		return
	}
	for _, rec := range recs {
		exists, err := backend.IndexExists(ctx, rec.AccountID, rec.ID)
		if err != nil {
			slog.Warn("Ochre vector store: reconcile indexes: check exists failed",
				"account", rec.AccountID, "index", rec.ID, "err", err)
			continue
		}
		if exists {
			continue
		}
		if err := backend.EnsureAccount(ctx, rec.AccountID); err != nil {
			slog.Warn("Ochre vector store: reconcile indexes: ensure account failed",
				"account", rec.AccountID, "index", rec.ID, "err", err)
			continue
		}
		spec := handlers_ochrevector.IndexSpec{ID: rec.ID, Dimension: rec.Dimension}
		if err := backend.CreateIndex(ctx, rec.AccountID, spec); err != nil {
			slog.Warn("Ochre vector store: reconcile indexes: create index failed",
				"account", rec.AccountID, "index", rec.ID, "err", err)
			continue
		}
		for _, source := range rec.SourceSpecs {
			if _, err := ingest.ReconcileFromSource(ctx, rec.AccountID, rec.ID, source); err != nil {
				slog.Warn("Ochre vector store: reconcile indexes: enqueue ingest failed",
					"account", rec.AccountID, "index", rec.ID, "err", err)
			}
		}
		slog.Info("Ochre vector store: reconcile indexes: re-created index absent from fresh appliance",
			"account", rec.AccountID, "index", rec.ID, "sources", len(rec.SourceSpecs))
	}
}

// ochreIngestSweepInterval paces the scheduler tick that drives PENDING ingest
// jobs and re-drives stale RUNNING ones.
const ochreIngestSweepInterval = 15 * time.Second

// runOchreIngestScheduler drives the ingestion sweep on a timer until the
// daemon context is cancelled. An initial sweep keeps submit latency low; the
// ticker then re-drives PENDING and crash-abandoned RUNNING jobs.
func (d *Daemon) runOchreIngestScheduler(ingest *handlers_ochrevector.IngestService) {
	if err := ingest.Sweep(d.ctx); err != nil {
		slog.Warn("Ochre vector store: initial ingest sweep", "err", err)
	}
	ticker := time.NewTicker(ochreIngestSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			if err := ingest.Sweep(d.ctx); err != nil {
				slog.Warn("Ochre vector store: ingest sweep", "err", err)
			}
		}
	}
}

// handleOchreApplianceTeardown answers SubjectTeardownAppliance: destroys the
// platform appliance singleton, then clears this daemon's own references so
// nothing keeps issuing queries against a torn-down backend. A daemon whose
// appliance never came up (disabled, still starting, or already torn down by
// an earlier call) has nothing to act on, which is reported as an error
// rather than silently accepted -- an operator asking to tear down expects
// one to have existed.
func (d *Daemon) handleOchreApplianceTeardown(ctx context.Context, req *handlers_ochrevector.TeardownApplianceRequest, _ string) (*handlers_ochrevector.TeardownApplianceResponse, error) {
	d.mu.Lock()
	appliance := d.ochreAppliance
	d.mu.Unlock()
	if appliance == nil {
		return nil, errors.New("ochrevector: platform appliance is not enabled or not up on this node")
	}

	purgeMetadata := req != nil && req.PurgeMetadata
	if err := appliance.Teardown(ctx, purgeMetadata); err != nil {
		return nil, fmt.Errorf("ochrevector: teardown platform appliance: %w", err)
	}

	d.mu.Lock()
	d.ochreAppliance = nil
	d.ochreVectorService = nil
	d.ochreBackupService = nil
	d.mu.Unlock()

	return &handlers_ochrevector.TeardownApplianceResponse{}, nil
}

// connectOchreAppliance drives Ensure-then-Connect until it succeeds or the
// daemon shuts down, so a torn-down-then-readopted appliance heals on its own
// rather than disabling the feature permanently after a few early failures.
func (d *Daemon) connectOchreAppliance(appliance *handlers_ochrevector.Appliance) (handlers_ochrevector.VectorBackend, error) {
	return retryUntilContext(d.ctx, ochreStartupInitialBackoff, ochreStartupMaxBackoff,
		func(attempt int, backoff time.Duration, err error) {
			if attempt <= ochreStartupLogAttempts || attempt%ochreStartupLogEvery == 0 {
				slog.Warn("Ochre vector store: appliance not reachable, will keep retrying",
					"attempt", attempt, "backoff_ms", otelsetup.Millis(backoff), "err", err)
			}
		},
		func() (handlers_ochrevector.VectorBackend, error) {
			return d.ensureAndConnectOchreApplianceOnce(appliance)
		})
}

// retryUntilContext calls attempt until it succeeds or ctx is cancelled,
// sleeping a doubling backoff (capped at maxBackoff) between failures and
// reporting each to log. No attempt ceiling: ctx cancellation is the only exit.
func retryUntilContext[T any](ctx context.Context, initialBackoff, maxBackoff time.Duration,
	log func(attempt int, backoff time.Duration, err error), attempt func() (T, error)) (T, error) {
	backoff := initialBackoff
	for n := 1; ; n++ {
		if err := ctx.Err(); err != nil {
			var zero T
			return zero, err
		}
		v, err := attempt()
		if err == nil {
			return v, nil
		}
		log(n, backoff, err)
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// ensureAndConnectOchreApplianceOnce runs a single Ensure-then-Connect
// attempt, bounding Ensure by ochreApplianceLaunchTimeout the same way the
// original one-shot call did; Connect is bounded by d.ctx alone, since it
// does its own network dial rather than a long poll loop.
func (d *Daemon) ensureAndConnectOchreApplianceOnce(appliance *handlers_ochrevector.Appliance) (handlers_ochrevector.VectorBackend, error) {
	ensureCtx, cancel := context.WithTimeout(d.ctx, ochreApplianceLaunchTimeout)
	defer cancel()
	if _, err := appliance.Ensure(ensureCtx); err != nil {
		return nil, fmt.Errorf("ensure platform appliance: %w", err)
	}
	backend, err := appliance.Connect(d.ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to platform appliance: %w", err)
	}
	return backend, nil
}
