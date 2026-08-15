package handlers_ochrevector

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/url"
	"sync"
	"time"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// applianceBucket is the cluster-replicated KV bucket holding the platform
// Postgres appliance's singleton record (D2): endpoint, credential and
// lifecycle state for the one control-plane-owned pgvector instance.
const applianceBucket = "ochre-vector-platform"

// applianceBucketHistory keeps one revision per key: the appliance record is
// one JSON document mutated in place, not a series.
const applianceBucketHistory = 1

// appliancePostgresKey is the fixed key every daemon races to Create,
// making the create itself the cluster-wide single-writer mutex.
const appliancePostgresKey = "postgres"

// ApplianceIdentifier is the appliance's reserved, deployment-wide identifier
// (D2): a system resource under utils.GlobalAccountID, never a
// caller-supplied or generated id.
const ApplianceIdentifier = "ochre-vector-pg"

// applianceMasterUsername is the fixed master role name the control plane
// connects as; it never has login privileges granted to any tenant.
const applianceMasterUsername = "ochre_vector_admin"

// AppliancePostgresDatabase is the single database the appliance serves;
// every account's schema (D3) lives inside it.
const AppliancePostgresDatabase = "postgres"

// Appliance lifecycle states (D2), analogous to the index registry's
// CREATING/READY but named for a long-lived singleton rather than a
// per-account resource.
const (
	ApplianceStateProvisioning = "PROVISIONING"
	ApplianceStateAvailable    = "AVAILABLE"
)

// SubjectTeardownAppliance is the operator-only NATS subject for destroying
// the platform appliance singleton (D2): reached solely via `spx admin ochre
// appliance teardown`, never through the tenant-facing awsgw surface.
const SubjectTeardownAppliance = "ochre.appliance.teardown"

// applianceStaleAfter bounds how long a PROVISIONING record may sit before a
// losing caller treats it as abandoned by a crashed winner and resumes the
// launch itself, rather than waiting on it forever.
const applianceStaleAfter = 5 * time.Minute

// appliancePollInterval paces a loser's wait loop between staleness/state
// re-checks while a winner is still provisioning.
const appliancePollInterval = 2 * time.Second

// appliancePasswordLength and appliancePasswordAlphabet define the generated
// master password: 32 characters from an alphanumeric-only alphabet, so the
// password never needs percent-encoding in a DSN or connection URL.
const appliancePasswordLength = 32

const appliancePasswordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// ApplianceRecord is the persisted singleton record for the platform Postgres
// appliance. EncryptedPassword is AES-256-GCM ciphertext (base64, via
// handlers_iam.EncryptSecret/DecryptSecret) — the plaintext master password
// is never stored, logged, or held anywhere outside a launch/Connect call.
//
// No exported Appliance method ever returns an ApplianceRecord: Ensure
// returns the password-free ApplianceConnInfo, and Connect resolves and
// discards the plaintext internally. The exported fields exist only so this
// package's own JSON encode/decode round-trips them.
type ApplianceRecord struct {
	Identifier        string    `json:"identifier"`
	Endpoint          string    `json:"endpoint,omitempty"`
	Port              int       `json:"port,omitempty"`
	MasterUsername    string    `json:"masterUsername"`
	EncryptedPassword string    `json:"encryptedPassword"`
	State             string    `json:"state"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// TeardownApplianceRequest has no fields: the appliance is a fixed,
// deployment-wide singleton (D2), never identified by a caller-supplied ID.
type TeardownApplianceRequest struct{}

type TeardownApplianceResponse struct{}

// ApplianceConnInfo is the appliance's connection endpoint: safe to log,
// return over an API, or hand to a caller freely, since it never carries the
// master password.
type ApplianceConnInfo struct {
	Identifier     string
	Endpoint       string
	Port           int
	MasterUsername string
}

// ApplianceLauncher is the seam to the real VM launch machinery: this stage
// fakes it in tests, a later stage wires it to handlers/rds's
// CreateDBInstance/LaunchDBInstanceVM. Launch must be identifier-idempotent
// downstream, since Ensure's re-entrant recovery path may call it again for
// an identifier it already launched.
type ApplianceLauncher interface {
	Launch(ctx context.Context, identifier, masterUsername, masterPassword string) (endpoint string, port int, err error)

	// Delete removes identifier's backing instance. Idempotent: an identifier
	// already gone (or never launched) is a no-op success, so Teardown never
	// fails on a resource it already reclaimed.
	Delete(ctx context.Context, identifier string) error
}

// Appliance owns the singleton platform Postgres appliance: its encrypted
// credential store and the re-entrant Ensure state machine over it (D2).
type Appliance struct {
	js        jetstream.JetStream
	masterKey []byte
	launcher  ApplianceLauncher

	mu sync.Mutex
	kv jetstream.KeyValue

	// hostPortDeps and eniID back this daemon's routed presence in the
	// appliance's VPC. A nil hostPortDeps.HostPort (WithHostPort never called)
	// makes Connect skip all host-port work, so existing callers are unaffected.
	hostPortDeps HostPortDeps
	eniID        string

	// registry and jobs are optional Teardown collaborators (WithStores): a
	// nil pair (the zero value, and every test constructing an Appliance
	// directly) skips the metadata purge, unchanged from before it existed.
	registry *Registry
	jobs     *JobStore
}

// NewAppliance constructs an Appliance over js, encrypting/decrypting the
// master password with masterKey and launching via launcher. masterKey must
// be non-empty: an appliance with no key to encrypt the credential it is
// about to generate is a configuration error, not a runtime one to defer.
func NewAppliance(js jetstream.JetStream, masterKey []byte, launcher ApplianceLauncher) (*Appliance, error) {
	if len(masterKey) == 0 {
		return nil, errors.New("ochrevector: appliance requires a master key to encrypt the platform postgres credential; none provided")
	}
	if launcher == nil {
		return nil, errors.New("ochrevector: appliance requires an ApplianceLauncher")
	}
	return &Appliance{js: js, masterKey: masterKey, launcher: launcher}, nil
}

// WithHostPort attaches this daemon's VPC host-port collaborators, so every
// future Connect first ensures a routed presence in the appliance's VPC
// before dialing it. Optional: an Appliance that never calls this performs
// no host-port work, which is what every caller not wired for the VPC fix
// (and every test that constructs an Appliance directly) gets by default.
func (a *Appliance) WithHostPort(deps HostPortDeps) *Appliance {
	a.mu.Lock()
	a.hostPortDeps = deps
	a.mu.Unlock()
	return a
}

// WithStores attaches the index registry and job store Teardown purges
// alongside the appliance's own record, so a rebuilt appliance starts with
// no stale metadata pointing at data that no longer exists. Optional: an
// Appliance that never calls this skips the purge (back-compat default).
func (a *Appliance) WithStores(registry *Registry, jobs *JobStore) *Appliance {
	a.mu.Lock()
	a.registry = registry
	a.jobs = jobs
	a.mu.Unlock()
	return a
}

// TeardownHostPort removes this daemon's own VPC host port, if Connect ever
// installed one. Not per-endpoint -- the port belongs to this node, not to
// any one Connect call -- so this is meant for daemon shutdown, not for
// unwinding every failed or short-lived Connect.
func (a *Appliance) TeardownHostPort() error {
	a.mu.Lock()
	deps := a.hostPortDeps
	eniID := a.eniID
	a.mu.Unlock()
	return removeApplianceHostPort(deps, eniID)
}

// Teardown removes the singleton platform appliance -- RDS instance, host
// port, then (if WithStores was called) index/job metadata, then the KV
// record -- so a rebuilt appliance starts coherent. Every step runs
// regardless of an earlier one's failure, and every failure is joined into
// the returned error. Idempotent overall: tearing down an already-absent
// appliance is a no-op success. Does not re-provision -- the daemon's own
// Ensure does that on next startup, not this call.
func (a *Appliance) Teardown(ctx context.Context) error {
	var errs []error

	if err := a.launcher.Delete(ctx, ApplianceIdentifier); err != nil {
		errs = append(errs, fmt.Errorf("ochrevector: delete appliance db instance: %w", err))
	}

	if err := a.TeardownHostPort(); err != nil {
		slog.Warn("ochrevector: appliance teardown: host port removal failed", "err", err)
		errs = append(errs, fmt.Errorf("ochrevector: remove appliance host port: %w", err))
	}

	a.mu.Lock()
	registry, jobs := a.registry, a.jobs
	a.mu.Unlock()
	// Purged after the data-bearing RDS instance is gone but before the
	// appliance record: a crash here still leaves a record an operator can
	// retry teardown against, re-attempting the (idempotent) purge too.
	if registry != nil {
		if err := registry.PurgeAll(ctx); err != nil {
			errs = append(errs, fmt.Errorf("ochrevector: purge index registry: %w", err))
		}
	}
	if jobs != nil {
		if err := jobs.PurgeAll(ctx); err != nil {
			errs = append(errs, fmt.Errorf("ochrevector: purge ingestion jobs: %w", err))
		}
	}

	kv, err := a.bucket(ctx)
	if err != nil {
		errs = append(errs, err)
		return errors.Join(errs...)
	}
	if err := kv.Delete(ctx, appliancePostgresKey); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		errs = append(errs, fmt.Errorf("ochrevector: delete appliance record: %w", err))
	}

	return errors.Join(errs...)
}

// ensureHostPort gives this daemon a routed presence in the appliance's VPC,
// called from Connect strictly before the DSN is built and dialed: the port
// binds an iface-id only after the ENI's logical switch port is programmed,
// which ensureDaemonENI does first. Returns the appliance's real dial IP, or
// "" as a no-op when WithHostPort was never called, so Connect's pre-VPC-fix
// dial path (rec.Endpoint) is unchanged for those callers.
func (a *Appliance) ensureHostPort(ctx context.Context, rec *ApplianceRecord) (string, error) {
	a.mu.Lock()
	deps := a.hostPortDeps
	a.mu.Unlock()
	if deps.HostPort == nil {
		return "", nil
	}
	dialIP, eniID, err := ensureApplianceHostPort(ctx, deps, rec.Identifier)
	if err != nil {
		return "", fmt.Errorf("ochrevector: ensure daemon host port: %w", err)
	}
	a.mu.Lock()
	a.eniID = eniID
	a.mu.Unlock()
	return dialIP, nil
}

// bucket lazily opens (or creates) the appliance KV bucket, caching the
// handle, mirroring Registry.bucket.
func (a *Appliance) bucket(ctx context.Context) (jetstream.KeyValue, error) {
	if a.js == nil {
		return nil, errors.New("ochrevector: appliance has no JetStream client configured")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kv != nil {
		return a.kv, nil
	}
	kv, err := kvutil.GetOrCreateBucket(ctx, a.js, applianceBucket, applianceBucketHistory)
	if err != nil {
		return nil, err
	}
	a.kv = kv
	return kv, nil
}

// Ensure is the re-entrant singleton-appliance orchestrator (D2): exactly one
// caller across a multi-node control plane wins the atomic KV claim and
// drives the launch; every other caller, concurrent or later, either waits
// for that launch or resumes a crashed one, and no caller ever generates a
// second password or identifier for the appliance.
func (a *Appliance) Ensure(ctx context.Context) (ApplianceConnInfo, error) {
	kv, err := a.bucket(ctx)
	if err != nil {
		return ApplianceConnInfo{}, err
	}

	rec, plaintext, err := newApplianceRecord(a.masterKey)
	if err != nil {
		return ApplianceConnInfo{}, err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return ApplianceConnInfo{}, fmt.Errorf("ochrevector: encode appliance record: %w", err)
	}

	rev, err := kv.Create(ctx, appliancePostgresKey, data)
	switch {
	case err == nil:
		// Winner: the single-writer claim succeeded, so this caller alone
		// drives the launch and the promotion to AVAILABLE.
		return a.launchAndPromote(ctx, kv, rec, rev, plaintext)
	case errors.Is(err, jetstream.ErrKeyExists):
		return a.waitOrResume(ctx, kv)
	default:
		return ApplianceConnInfo{}, fmt.Errorf("ochrevector: claim appliance: %w", err)
	}
}

// launchAndPromote runs the winner's path: launch the real instance, then
// flip the record to AVAILABLE. A launch failure leaves the record
// PROVISIONING rather than rolling it back — retrying immediately with a
// fresh claim would mint a second password for the same singleton appliance,
// which Ensure must never do; the staleness-triggered resume path is the
// only recovery.
func (a *Appliance) launchAndPromote(ctx context.Context, kv jetstream.KeyValue, rec ApplianceRecord, rev uint64, plaintext string) (ApplianceConnInfo, error) {
	endpoint, port, err := a.launcher.Launch(ctx, rec.Identifier, rec.MasterUsername, plaintext)
	if err != nil {
		return ApplianceConnInfo{}, fmt.Errorf("ochrevector: launch appliance: %w", err)
	}
	return a.promote(ctx, kv, rec, rev, endpoint, port)
}

// promote flips rec to AVAILABLE via a revision-guarded (CAS) update. If the
// update loses a race to a concurrent resume of the same stale record, a
// record that has already reached AVAILABLE is this caller's success too,
// not a conflict to surface.
func (a *Appliance) promote(ctx context.Context, kv jetstream.KeyValue, rec ApplianceRecord, rev uint64, endpoint string, port int) (ApplianceConnInfo, error) {
	rec.State = ApplianceStateAvailable
	rec.Endpoint = endpoint
	rec.Port = port
	rec.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(rec)
	if err != nil {
		return ApplianceConnInfo{}, fmt.Errorf("ochrevector: encode appliance record: %w", err)
	}
	if _, err := kv.Update(ctx, appliancePostgresKey, data, rev); err != nil {
		if current, _, getErr := a.getRecord(ctx, kv); getErr == nil && current != nil && current.State == ApplianceStateAvailable {
			return connInfo(*current), nil
		}
		return ApplianceConnInfo{}, fmt.Errorf("ochrevector: promote appliance to available: %w", err)
	}
	return connInfo(rec), nil
}

// waitOrResume is the losing caller's path: read the winner's record and
// either return its endpoint once AVAILABLE, resume a stale crashed
// provision, or wait and re-check, bounded by ctx.
func (a *Appliance) waitOrResume(ctx context.Context, kv jetstream.KeyValue) (ApplianceConnInfo, error) {
	for {
		rec, entry, err := a.getRecord(ctx, kv)
		if err != nil {
			return ApplianceConnInfo{}, err
		}
		if rec == nil {
			// The record vanished between our lost claim and this read (e.g.
			// an operator delete); retry the claim from scratch.
			return a.Ensure(ctx)
		}
		switch rec.State {
		case ApplianceStateAvailable:
			return connInfo(*rec), nil
		case ApplianceStateProvisioning:
			if time.Since(rec.UpdatedAt) > applianceStaleAfter {
				return a.resume(ctx, kv, *rec, entry.Revision())
			}
		default:
			return ApplianceConnInfo{}, fmt.Errorf("ochrevector: appliance record in unexpected state %q", rec.State)
		}
		select {
		case <-ctx.Done():
			return ApplianceConnInfo{}, fmt.Errorf("ochrevector: wait for appliance provisioning: %w", ctx.Err())
		case <-time.After(appliancePollInterval):
		}
	}
}

// resume re-drives the launch for a stale PROVISIONING record left by a
// crashed winner, reusing rec's existing identifier and stored password
// rather than generating either anew (D2), then promotes it.
func (a *Appliance) resume(ctx context.Context, kv jetstream.KeyValue, rec ApplianceRecord, rev uint64) (ApplianceConnInfo, error) {
	password, err := a.decryptPassword(&rec)
	if err != nil {
		return ApplianceConnInfo{}, err
	}
	endpoint, port, err := a.launcher.Launch(ctx, rec.Identifier, rec.MasterUsername, password)
	if err != nil {
		return ApplianceConnInfo{}, fmt.Errorf("ochrevector: resume stale appliance provisioning: %w", err)
	}
	return a.promote(ctx, kv, rec, rev, endpoint, port)
}

// Connect builds a ready *pgxBackend for the current AVAILABLE appliance
// record: the sole accessor that decrypts the master password, and only for
// the moment it takes to compose the DSN and open the pool. The password
// never leaves this call.
func (a *Appliance) Connect(ctx context.Context) (*pgxBackend, error) {
	kv, err := a.bucket(ctx)
	if err != nil {
		return nil, err
	}
	rec, _, err := a.getRecord(ctx, kv)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errors.New("ochrevector: appliance not yet provisioned")
	}
	if rec.State != ApplianceStateAvailable {
		return nil, fmt.Errorf("ochrevector: appliance not available (state %q)", rec.State)
	}

	dialIP, err := a.ensureHostPort(ctx, rec)
	if err != nil {
		return nil, err
	}

	password, err := a.decryptPassword(rec)
	if err != nil {
		return nil, err
	}
	// dialIP is only ever non-empty when the host-port path actually ran
	// (WithHostPort configured); otherwise this is the unchanged pre-fix
	// dial target.
	dialHost := rec.Endpoint
	if dialIP != "" {
		dialHost = dialIP
	}
	dsn := buildDSN(dialHost, rec.Port, rec.MasterUsername, password)

	backend, err := NewPgxBackend(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := backend.Init(ctx); err != nil {
		backend.Close()
		return nil, err
	}
	return backend, nil
}

// getRecord reads and decodes the appliance record, returning (nil, nil, nil)
// when absent.
func (a *Appliance) getRecord(ctx context.Context, kv jetstream.KeyValue) (*ApplianceRecord, jetstream.KeyValueEntry, error) {
	entry, err := kv.Get(ctx, appliancePostgresKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("ochrevector: get appliance record: %w", err)
	}
	var rec ApplianceRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, nil, fmt.Errorf("ochrevector: decode appliance record: %w", err)
	}
	return &rec, entry, nil
}

// decryptPassword resolves rec's encrypted master password to plaintext. The
// error path never includes the ciphertext or any decrypted material.
func (a *Appliance) decryptPassword(rec *ApplianceRecord) (string, error) {
	plaintext, err := handlers_iam.DecryptSecret(rec.EncryptedPassword, a.masterKey)
	if err != nil {
		return "", fmt.Errorf("ochrevector: decrypt appliance master password: %w", err)
	}
	return plaintext, nil
}

// newApplianceRecord builds a fresh PROVISIONING record with a newly
// generated master password, returning both the record (ciphertext only) and
// the plaintext for the caller's immediate use in the winning launch call.
func newApplianceRecord(masterKey []byte) (ApplianceRecord, string, error) {
	password, err := generateMasterPassword()
	if err != nil {
		return ApplianceRecord{}, "", err
	}
	encrypted, err := handlers_iam.EncryptSecret(password, masterKey)
	if err != nil {
		return ApplianceRecord{}, "", fmt.Errorf("ochrevector: encrypt appliance master password: %w", err)
	}
	now := time.Now().UTC()
	return ApplianceRecord{
		Identifier:        ApplianceIdentifier,
		MasterUsername:    applianceMasterUsername,
		EncryptedPassword: encrypted,
		State:             ApplianceStateProvisioning,
		CreatedAt:         now,
		UpdatedAt:         now,
	}, password, nil
}

// generateMasterPassword returns a strong, DSN/URL-safe random password:
// appliancePasswordLength characters drawn uniformly from
// appliancePasswordAlphabet via crypto/rand, so it never needs
// percent-encoding when composed into a connection URL.
func generateMasterPassword() (string, error) {
	alphabetLen := big.NewInt(int64(len(appliancePasswordAlphabet)))
	result := make([]byte, appliancePasswordLength)
	for i := range result {
		n, err := rand.Int(rand.Reader, alphabetLen)
		if err != nil {
			return "", fmt.Errorf("ochrevector: generate appliance master password: %w", err)
		}
		result[i] = appliancePasswordAlphabet[n.Int64()]
	}
	return string(result), nil
}

// connInfo projects rec to its password-free connection info.
func connInfo(rec ApplianceRecord) ApplianceConnInfo {
	return ApplianceConnInfo{
		Identifier:     rec.Identifier,
		Endpoint:       rec.Endpoint,
		Port:           rec.Port,
		MasterUsername: rec.MasterUsername,
	}
}

// buildDSN composes a postgres connection URL, URL-encoding user/password via
// net/url so even an unexpected special character round-trips safely rather
// than corrupting the DSN.
func buildDSN(endpoint string, port int, username, password string) string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(username, password),
		Host:   fmt.Sprintf("%s:%d", endpoint, port),
		Path:   "/" + AppliancePostgresDatabase,
	}
	return u.String()
}
