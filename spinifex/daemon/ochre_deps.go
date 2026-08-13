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
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_ochrevector "github.com/mulgadc/spinifex/spinifex/handlers/ochrevector"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/nats-io/nats.go/jetstream"
)

// ochreApplianceLaunchTimeout bounds the platform Postgres appliance's whole
// create-and-poll-to-available launch: generous enough for a cold RDS VM
// boot plus initdb on the smallest instance class.
const ochreApplianceLaunchTimeout = 10 * time.Minute

// ochreStartupMaxAttempts bounds how many times startOchreVector retries the
// Ensure-then-Connect pair. A still-provisioning appliance or a host port
// not yet routed is transient, but the retry still needs a ceiling so a
// genuine misconfiguration eventually gives up instead of retrying forever.
const ochreStartupMaxAttempts = 5

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

	// registry and jobs need only js, never a live backend, so they are built
	// (and attached) here rather than after Connect: Teardown must be able to
	// purge them even for an appliance that never comes up. Reused below,
	// once connected, rather than reconstructed.
	registry := handlers_ochrevector.NewRegistry(js)
	jobs := handlers_ochrevector.NewJobStore(js)
	appliance.WithStores(registry, jobs)

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
		slog.Warn("Ochre vector store disabled: platform appliance not reachable after retrying",
			"attempts", ochreStartupMaxAttempts, "err", err)
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

	// registry and jobs were already built above (and attached to appliance
	// via WithStores) before Connect was attempted; reused here, not rebuilt.
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
		{handlers_ochrevector.SubjectListJobs, handleNATSRequest(vectorService.ListJobs), "spinifex-workers"},
	}
	if err := d.registerNatsSubs(subs); err != nil {
		slog.Error("Ochre vector store: failed to register NATS subjects", "err", err)
		return
	}

	// Assigned last, and only after every subject above is live: a reader
	// (there is none today beyond observability) must never see a non-nil
	// service whose subjects are not yet actually serving.
	d.ochreVectorService = vectorService
	go d.runOchreIngestScheduler(ingest)
	slog.Info("Ochre vector store enabled")
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
func (d *Daemon) handleOchreApplianceTeardown(ctx context.Context, _ *handlers_ochrevector.TeardownApplianceRequest, _ string) (*handlers_ochrevector.TeardownApplianceResponse, error) {
	d.mu.Lock()
	appliance := d.ochreAppliance
	d.mu.Unlock()
	if appliance == nil {
		return nil, errors.New("ochrevector: platform appliance is not enabled or not up on this node")
	}

	if err := appliance.Teardown(ctx); err != nil {
		return nil, fmt.Errorf("ochrevector: teardown platform appliance: %w", err)
	}

	d.mu.Lock()
	d.ochreAppliance = nil
	d.ochreVectorService = nil
	d.mu.Unlock()

	return &handlers_ochrevector.TeardownApplianceResponse{}, nil
}

// connectOchreAppliance drives Ensure then Connect as one retryable unit, up
// to ochreStartupMaxAttempts times with exponential backoff between
// attempts. Previously a single provisioning timeout -- the appliance still
// booting, or its host port not yet routed -- hard-disabled the feature for
// the daemon's entire lifetime; this bounds that to a handful of attempts
// instead of exactly one, while still eventually giving up on a persistent
// failure rather than retrying forever.
func (d *Daemon) connectOchreAppliance(appliance *handlers_ochrevector.Appliance) (handlers_ochrevector.VectorBackend, error) {
	backoff := ochreStartupInitialBackoff
	var lastErr error
	for attempt := 1; attempt <= ochreStartupMaxAttempts; attempt++ {
		if d.ctx.Err() != nil {
			return nil, d.ctx.Err()
		}

		backend, err := d.ensureAndConnectOchreApplianceOnce(appliance)
		if err == nil {
			return backend, nil
		}
		lastErr = err

		if attempt == ochreStartupMaxAttempts {
			break
		}
		slog.Warn("Ochre vector store: appliance not ready, retrying",
			"attempt", attempt, "max_attempts", ochreStartupMaxAttempts, "backoff", backoff, "err", lastErr)
		select {
		case <-d.ctx.Done():
			return nil, d.ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > ochreStartupMaxBackoff {
			backoff = ochreStartupMaxBackoff
		}
	}
	return nil, lastErr
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
