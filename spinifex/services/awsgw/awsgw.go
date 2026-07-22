package awsgw

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mulgadc/predastore/ratelimit"
	"github.com/mulgadc/spinifex/internal/tlsconfig"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
	gateway_ecr "github.com/mulgadc/spinifex/spinifex/gateway/ecr"
	gateway_ecrauth "github.com/mulgadc/spinifex/spinifex/gateway/ecrauth"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/network/reconcile"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	toml "github.com/pelletier/go-toml/v2"
)

var serviceName = "awsgw"

// Version and Commit are set by the cmd package before Start() to pass
// build-time ldflags to the gateway without creating an import cycle.
var (
	version = "dev"
	commit  = "unknown"
)

// SetBuildInfo sets the build-time version and commit for the gateway.
// Call before Start().
func SetBuildInfo(v, c string) {
	version = v
	commit = c
}

type Service struct {
	Config *config.ClusterConfig
}

func New(cfg any) (svc *Service, err error) {
	c, ok := cfg.(*config.ClusterConfig)
	if !ok {
		return nil, fmt.Errorf("invalid config type for awsgw service")
	}
	svc = &Service{
		Config: c,
	}
	return svc, nil
}

func (svc *Service) Start() (int, error) {
	if err := utils.WritePidFileTo(svc.Config.NodeBaseDir(), serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}
	err := launchService(svc.Config)
	if err != nil {
		return 0, err
	}

	return os.Getpid(), nil
}

func (svc *Service) Stop() (err error) {
	return utils.StopProcessAt(svc.Config.NodeBaseDir(), serviceName)
}

func (svc *Service) Status() (string, error) {
	return utils.ServiceStatus(svc.Config.NodeBaseDir(), serviceName)
}

func (svc *Service) Shutdown() (err error) {
	return svc.Stop()
}

func (svc *Service) Reload() (err error) {
	return nil
}

// awsgwTOML is the top-level structure of awsgw.toml used to extract the
// ratelimit and quota sections. Other fields are parsed elsewhere (e.g. region,
// debug).
type awsgwTOML struct {
	Ratelimit ratelimit.Config      `toml:"ratelimit"`
	Quota     handlers_quota.Limits `toml:"quota"`
}

// loadAWSGWConfig reads and parses awsgw.toml once, returning the [ratelimit] and
// [quota] sections together. Both sections default to their zero value (a
// disabled no-op) when absent, so a config without either block stays valid.
func loadAWSGWConfig(path string) (awsgwTOML, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return awsgwTOML{}, fmt.Errorf("read awsgw config %s: %w", path, err)
	}
	var cfg awsgwTOML
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return awsgwTOML{}, fmt.Errorf("parse awsgw config %s: %w", path, err)
	}
	return cfg, nil
}

// openAccountUsageBucket opens (or idempotently creates) the gateway-owned
// per-account vCPU usage bucket. History is 1: each account key holds a single
// CAS-updated integer counter. It attaches first and creates only when the bucket
// is genuinely absent, so a transient create error is not masked by the fallback.
func openAccountUsageBucket(js nats.JetStreamContext, replicas int) (nats.KeyValue, error) {
	if replicas < 1 {
		replicas = 1
	}
	kv, err := js.KeyValue(handlers_quota.KVBucketAccountUsage)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{
			Bucket:   handlers_quota.KVBucketAccountUsage,
			History:  1,
			Replicas: replicas,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("open account usage bucket: %w", err)
	}
	return kv, nil
}

func launchService(config *config.ClusterConfig) error {
	nodeConfig := config.Nodes[config.Node]

	natsConn, err := utils.ConnectNATSWithRetry(admin.DialTarget(nodeConfig.NATS.Host), nodeConfig.NATS.ACL.Token, nodeConfig.NATS.CACert)
	if err != nil {
		return err
	}
	defer natsConn.Close()

	// Append Base dir if config has no leading path
	if nodeConfig.BaseDir != "" && !strings.HasPrefix(nodeConfig.AWSGW.Config, "/") {
		nodeConfig.AWSGW.Config = fmt.Sprintf("%s/%s", nodeConfig.BaseDir, nodeConfig.AWSGW.Config)
	}

	// Load IAM master key from disk (required for all authenticated requests)
	masterKeyPath := filepath.Join(nodeConfig.BaseDir, "config", "master.key")
	masterKey, err := handlers_iam.LoadMasterKey(masterKeyPath)
	if err != nil {
		return fmt.Errorf("load IAM master key from %s: %w", masterKeyPath, err)
	}

	// Initialize IAM service with NATS KV backend (required for auth).
	// On multi-node clusters, JetStream KV requires cluster quorum which may
	// not be available yet if nodes start concurrently. Retry with backoff.
	iamService, err := handlers_iam.NewIAMServiceWithRetry(natsConn, masterKey, len(config.Nodes))
	if err != nil {
		return fmt.Errorf("initialize IAM service: %w", err)
	}

	// STS service shares the IAM master key (single envelope for at-rest
	// secrets + session-token HMACs) and resolves roles via IAMService.
	stsService, err := handlers_sts.NewSTSServiceImpl(natsConn, iamService, masterKey, len(config.Nodes))
	if err != nil {
		return fmt.Errorf("initialize STS service: %w", err)
	}

	// Janitor sweeps expired session credentials. Bound to the process
	// lifetime — the server below blocks until exit, so cancelling on return
	// is sufficient to let the goroutine drain.
	janitorCtx, cancelJanitor := context.WithCancel(context.Background())
	defer cancelJanitor()
	go stsService.RunJanitor(janitorCtx)

	// IMDS serves 169.254.169.254 to guest VMs from vpcd, which holds the network
	// capabilities the hardened awsgw sandbox can't grant. awsgw stays the home of
	// STS + IAM, answering the IMDS handler's control-plane RPCs over NATS.
	if _, err := stsService.SubscribeIMDSResponder(natsConn); err != nil {
		return fmt.Errorf("subscribe IMDS STS responder: %w", err)
	}
	if _, err := iamService.SubscribeIMDSResponders(natsConn); err != nil {
		return fmt.Errorf("subscribe IMDS IAM responders: %w", err)
	}

	// Expose the in-process STS presigned-URL verify over NATS for the
	// in-cluster eks-token-webhook (STS is gateway-local, not otherwise on the
	// bus). Bound to the process lifetime via natsConn.Close on return.
	tokenVerifySub, err := registerEKSTokenVerify(natsConn, stsService)
	if err != nil {
		return fmt.Errorf("subscribe EKS token verify: %w", err)
	}
	defer func() {
		if err := tokenVerifySub.Unsubscribe(); err != nil {
			slog.Warn("EKS token verify: unsubscribe failed", "err", err)
		}
	}()

	// First boot: consume bootstrap.json → seed IAM users into NATS KV → delete file.
	// Check data directory first (production: /var/lib/spinifex/awsgw/), then
	// awsgw subdir (dev: ~/spinifex/awsgw/), then legacy config dir.
	bootstrapPath := findBootstrapFile(nodeConfig.BaseDir)
	data, err := handlers_iam.LoadBootstrapData(bootstrapPath)
	switch {
	case err == nil:
		slog.Info("Bootstrap file found, seeding IAM users")
		if err := iamService.SeedBootstrap(data); err != nil {
			return fmt.Errorf("seed bootstrap from bootstrap.json: %w", err)
		}
		if err := os.Remove(bootstrapPath); err != nil {
			slog.Warn("Failed to delete bootstrap file", "path", bootstrapPath, "err", err)
		} else {
			slog.Info("Bootstrap complete, bootstrap.json deleted")
		}
	case os.IsNotExist(err):
		// No bootstrap file — normal after first boot
	default:
		return fmt.Errorf("load bootstrap from %s: %w", bootstrapPath, err)
	}

	// Load the awsgw config once: [ratelimit] throttling and [quota] per-account
	// service quotas. A load error leaves both at their zero value (disabled).
	awsgwTomlPath := filepath.Join(nodeConfig.BaseDir, "config", "awsgw", "awsgw.toml")
	awsgwCfg, err := loadAWSGWConfig(awsgwTomlPath)
	if err != nil {
		slog.Warn("Failed to load awsgw config, throttling and quotas disabled", "err", err)
	}
	throttleCfg := awsgwCfg.Ratelimit
	quotaCfg := awsgwCfg.Quota

	// OCI Distribution v2 registry: blob/manifest bytes stream straight to
	// predastore from the gateway; repo/tag/manifest metadata and in-progress
	// uploads are owned by the daemon and reached over NATS request/reply. The
	// /v2 auth bridge resolves the per-request account from a verified token.
	ecrStore := objectstore.NewS3ObjectStoreFromConfig(
		admin.DialTarget(nodeConfig.Predastore.Host),
		nodeConfig.Predastore.Region,
		nodeConfig.Predastore.AccessKey,
		nodeConfig.Predastore.SecretKey,
	)
	ecrRegistry := gateway_ecr.NewRegistry(ecrStore, ecr.NewNATSMetaStore(natsConn), config.Bootstrap.AccountID)

	// Lifecycle expiry sweep applies each repo's stored lifecycle policy and
	// deletes the expired set via the registry GC path. It runs here (not the
	// daemon) because only the gateway holds the object store. Bound to the same
	// lifetime context as the STS janitor.
	lifecycleSweeper := gateway_ecr.NewLifecycleSweeper(
		ecrRegistry, activeAccountIDs(iamService), gateway_ecr.DefaultLifecycleSweepInterval)
	go lifecycleSweeper.Run(janitorCtx)

	// ECR auth bridge: load (or first-run create) the ES256 signing key from the
	// cluster-replicated awsgw-keys KV bucket, then build the token issuer
	// (GetAuthorizationToken) and verifier (/v2 Authorization).
	js, err := natsConn.JetStream()
	if err != nil {
		return fmt.Errorf("ECR auth bridge: JetStream context: %w", err)
	}
	signingKey, verifyKeys, err := gateway_ecrauth.LoadOrCreateSigningKey(js, masterKey, len(config.Nodes))
	if err != nil {
		return fmt.Errorf("ECR auth bridge: load signing key: %w", err)
	}
	ecrAudience := "ecr." + nodeConfig.Region + "." + config.AWS.InternalSuffix

	// The ECR registry is served on this gateway's own host:port; advertise both so
	// docker login/tag/push reach it without DNS — the account comes from the auth
	// token. Prefer a concrete AWSGW bind host; when it is unspecified (0.0.0.0/::)
	// fall back to AdvertiseIP, the off-host dial target carried in the server cert
	// SANs (the same host EKS workers dial), so the returned URI resolves without
	// DNS. Only when neither is concrete does the per-account parity name apply.
	registryHost, registryPort := "", ""
	if host, port, err := net.SplitHostPort(nodeConfig.AWSGW.Host); err == nil {
		registryPort = port
		if isConcreteRegistryHost(host) {
			registryHost = host
		}
	}
	if registryHost == "" && isConcreteRegistryHost(nodeConfig.AdvertiseIP) {
		registryHost = nodeConfig.AdvertiseIP
	}

	// Bedrock provider credentials: per-account keys live in the
	// bedrock-credentials KV bucket; OCHRE_ANTHROPIC_API_KEY seeds an optional
	// platform-wide default so accounts work before onboarding their own key.
	bedrockPlatformDefaults := map[string]string{}
	if key := os.Getenv("OCHRE_ANTHROPIC_API_KEY"); key != "" {
		bedrockPlatformDefaults["anthropic"] = key
	}
	bedrockCredentials := gateway_bedrock.NewCredentialStore(js, masterKey, len(config.Nodes), bedrockPlatformDefaults)

	// Bedrock model access: grants live in the bedrock-model-access KV bucket
	// and are deny-by-default, so a fresh deployment serves no models until an
	// operator grants them (spx admin ochre access grant).
	bedrockAccess := gateway_bedrock.NewModelAccessStore(js, len(config.Nodes))

	// Deny-by-default would otherwise leave a fresh install with a catalog
	// nobody can see, so seed the platform admin account — the operator's own
	// account, created by spx admin init — with the full catalog on first
	// start. Tenant accounts are unaffected and still begin with no access.
	// Best-effort: the gateway must serve even if this fails, and because the
	// marker is written only on success, the next start retries.
	if seeded, err := bedrockAccess.SeedAccountGrants(context.Background(), admin.DefaultAccountID(), gateway_bedrock.CatalogModelIDs()); err != nil {
		slog.Warn("Bedrock model access: seeding admin grants failed, will retry on next start",
			"accountID", admin.DefaultAccountID(), "err", err)
	} else if seeded {
		slog.Info("Bedrock model access: seeded admin account with the model catalog",
			"accountID", admin.DefaultAccountID(), "models", len(gateway_bedrock.CatalogModelIDs()))
	}

	// Bedrock self-host endpoints: Phase 1 models are pinned, so their
	// OpenAI-compatible base URLs come from static config. OCHRE_VLLM_ENDPOINTS
	// is a comma-separated list of modelId=baseURL pairs.
	bedrockEndpoints := parseBedrockEndpoints(os.Getenv("OCHRE_VLLM_ENDPOINTS"))

	gw := gateway.GatewayConfig{
		Debug:              nodeConfig.AWSGW.Debug,
		DisableLogging:     false,
		NATSConn:           natsConn,
		Config:             nodeConfig.AWSGW.Config,
		ExpectedNodes:      len(config.Nodes),
		Region:             nodeConfig.Region,
		InternalSuffix:     config.AWS.InternalSuffix,
		RegistryPort:       registryPort,
		RegistryHost:       registryHost,
		AZ:                 nodeConfig.AZ,
		IAMService:         iamService,
		STSService:         stsService,
		Version:            version,
		Commit:             commit,
		ECRRegistry:        ecrRegistry,
		ECRTokenIssuer:     gateway_ecrauth.NewIssuer(signingKey, ecrAudience),
		ECRTokenVerifier:   gateway_ecrauth.NewVerifier(verifyKeys, ecrAudience),
		BedrockCredentials: bedrockCredentials,
		BedrockEndpoints:   bedrockEndpoints,
		BedrockAccess:      bedrockAccess,
		BedrockAccessAdmin: bedrockAccess,
	}

	// Rotate the ECR signing key on a 30-day cadence, retaining the previous keys
	// until their tokens expire. The rotator keeps the issuer/verifier current as
	// keys roll. Bound to the same lifetime context as the STS janitor.
	keyRotator, err := gateway_ecrauth.NewRotator(js, masterKey, len(config.Nodes), gw.ECRTokenIssuer, gw.ECRTokenVerifier)
	if err != nil {
		return fmt.Errorf("ECR auth bridge: signing-key rotator: %w", err)
	}
	go keyRotator.Run(janitorCtx)

	if throttleCfg.Enabled {
		gw.Throttler = ratelimit.New(throttleCfg)
		defer gw.Throttler.Stop()
	}

	// Per-account service quotas. Only the enabled path opens the gateway-owned
	// usage KV bucket, leaving existing default-off gateways untouched; a disabled
	// config builds a no-op Service whose Exempt short-circuits every check.
	var usageBucket nats.KeyValue
	if quotaCfg.Enabled {
		usageBucket, err = openAccountUsageBucket(js, max(len(config.Nodes), 1))
		if err != nil {
			return fmt.Errorf("init account usage bucket: %w", err)
		}
	}
	gw.Quota = handlers_quota.New(quotaCfg, usageBucket)

	// Leader-locked vCPU reconcile: the only path that lowers the counter,
	// recomputing it from the running-plus-stopped sweep so out-of-band
	// terminations free quota. Started only when quotas are enabled so default-off
	// gateways spin no ticker.
	if quotaCfg.Enabled {
		// Sweep against the configured node total, not the live-active count: a
		// node that is down must make the sweep incomplete so reconcile leaves
		// the counter alone rather than lowering it from a partial view.
		expectedNodes := func() int { return len(config.Nodes) }
		go runQuotaReconcile(janitorCtx, gw.Quota, natsConn, activeAccountIDs(iamService), expectedNodes)
	}

	handler := gw.SetupRoutes()

	// Load TLS certificate
	cert, err := tls.LoadX509KeyPair(nodeConfig.AWSGW.TLSCert, nodeConfig.AWSGW.TLSKey)
	if err != nil {
		return fmt.Errorf("load TLS cert: %w", err)
	}

	server := &http.Server{
		Addr:              nodeConfig.AWSGW.Host,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			Certificates:     []tls.Certificate{cert},
			NextProtos:       []string{"h2", "http/1.1"},
			MinVersion:       tls.VersionTLS13,
			CurvePreferences: tlsconfig.Curves,
		},
	}

	slog.Info("AWS Gateway listening", "addr", nodeConfig.AWSGW.Host)
	if err := server.ListenAndServeTLS("", ""); err != nil {
		slog.Error("Failed to start TLS listener", "err", err)
		os.Exit(1)
	}

	return nil
}

// runQuotaReconcile drives the per-account vCPU reconcile: a startup pass plus a
// ReconcileInterval ticker, each guarded by the dedicated quota reconcile leader
// lock so exactly one gateway sweeps at a time across a multi-gateway deployment.
// The lock is distinct from vpcd's network-reconcile lock so the two loops never
// block each other. It runs until ctx is cancelled.
func runQuotaReconcile(ctx context.Context, quota *handlers_quota.Service, natsConn *nats.Conn, accounts handlers_quota.AccountLister, expectedNodes func() int) {
	holder, _ := os.Hostname()
	list := handlers_quota.NATSInstanceLister(natsConn, expectedNodes)

	runPass := func() {
		release, elected := reconcile.AcquireLeader(natsConn, handlers_quota.KVBucketQuotaReconcile, holder)
		if !elected {
			return
		}
		defer release()
		if err := quota.Reconcile(ctx, accounts, list); err != nil {
			slog.Warn("quota reconcile pass failed", "err", err)
		}
	}

	runPass()
	ticker := time.NewTicker(handlers_quota.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runPass()
		}
	}
}

// accountLister is the slice of IAMService the lifecycle sweeper needs.
type accountLister interface {
	ListAccounts() ([]*handlers_iam.Account, error)
}

// activeAccountIDs adapts IAMService.ListAccounts into the account-ID enumerator
// the ECR lifecycle sweeper expects, including only ACTIVE accounts.
func activeAccountIDs(iam accountLister) func() ([]string, error) {
	return func() ([]string, error) {
		accounts, err := iam.ListAccounts()
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(accounts))
		for _, acct := range accounts {
			if acct.Status == handlers_iam.AccountStatusActive {
				ids = append(ids, acct.AccountID)
			}
		}
		return ids, nil
	}
}

// findBootstrapFile returns the first existing bootstrap.json candidate path,
// or the primary path if none exist.
func findBootstrapFile(baseDir string) string {
	candidates := []string{
		filepath.Join(baseDir, "bootstrap.json"),
		filepath.Join(baseDir, "awsgw", "bootstrap.json"),
		filepath.Join(baseDir, "config", "bootstrap.json"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return candidates[0]
}

// isConcreteRegistryHost reports whether host is a dialable address to advertise
// as the ECR registry host — a non-empty, non-unspecified literal. The wildcard
// bind addresses are rejected so the registry URI never hands back 0.0.0.0/::.
func isConcreteRegistryHost(host string) bool {
	return host != "" && host != "0.0.0.0" && host != "::"
}

// parseBedrockEndpoints parses a comma-separated list of modelId=baseURL pairs
// into a map for the bedrock self-host endpoint resolver. Malformed or empty
// entries are skipped. Returns nil for empty input.
func parseBedrockEndpoints(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	endpoints := map[string]string{}
	for pair := range strings.SplitSeq(raw, ",") {
		modelID, baseURL, ok := strings.Cut(pair, "=")
		modelID = strings.TrimSpace(modelID)
		baseURL = strings.TrimSpace(baseURL)
		if !ok || modelID == "" || baseURL == "" {
			slog.Warn("awsgw: skipping malformed OCHRE_VLLM_ENDPOINTS entry", "entry", pair)
			continue
		}
		endpoints[modelID] = baseURL
	}
	if len(endpoints) == 0 {
		return nil
	}
	return endpoints
}
