package handlers_rds

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Host-side capabilities the RDS control plane needs beyond NATS.
type Deps struct {
	// The cluster CA the serving certs are signed by. Empty disables minting,
	// and GetDBBootstrapConfig returns no cert rather than failing the boot.
	CACertPath string
	CAKeyPath  string
	// Overrides the file-backed CA loader in tests.
	LoadCA CALoader
	// Overrides the size of the RSA key each serving cert is minted with. Zero
	// takes defaultServingCertKeyBits, which is what production runs.
	ServingCertKeyBits int
	// The cluster master key the staged bootstrap password is encrypted under.
	// Mandatory: without it a create cannot stage a password at all, so it is
	// resolved at construction rather than per operation.
	MasterKey []byte
	Launch    LaunchDeps
	// Resolves the customer subnet and validates the security groups the
	// customer ENI is created with.
	Network networkResolver
	// Find-or-creates the rdsInstanceRole instance profile granted at launch.
	IAM IAMProvider
	// Lets the reconciler confirm a DB VM is running before calling its
	// instance available. Nil leaves the transition on the heartbeat alone.
	InstanceState InstanceStateResolver
	// Drives the DB VM's power state for reboot, stop and start.
	Instances instanceCommander
	// Takes the final snapshot at delete, and reports which snapshots still
	// reference a data volume before it can be deleted.
	Snapshots snapshotProvider
	// Grows the data volume behind an AllocatedStorage modify.
	Storage volumeResizer
	// The northstar base zone. Empty means no vanity hostname, and the endpoint
	// is the customer-ENI IP instead.
	BaseDomain string
	// Identifies this node in the reconciler lease.
	HolderID string
	// Seeded into the DB VM's cloud-init so the in-guest agent can reach the
	// gateway and pin its TLS. No credentials: those come from IMDS.
	GatewayURL    string
	GatewayCACert string
	// Overrides how long the reconciler waits for the first healthy heartbeat.
	// Zero takes defaultBootstrapTimeout.
	BootstrapTimeout time.Duration
	// Overrides how long an instance may be observed dark before the classifier
	// calls it failed. Zero takes defaultFailureGrace.
	FailureGrace time.Duration
	// Overrides how long a stop waits for the fleet to report the VM down.
	// Zero takes defaultVMStopTimeout.
	VMStopTimeout time.Duration
	// Overrides how long an apply-params command waits for the agent to
	// answer. Zero takes defaultApplyParamsTimeout.
	ApplyParamsTimeout time.Duration
	// Override the modify lease lifetime and renewal cadence in tests. A zero
	// TTL takes the default; a refresh outside (0, TTL) derives as TTL/3.
	ModifyLeaseTTL     time.Duration
	ModifyLeaseRefresh time.Duration
	// Bounds and defaults for automated backups and the two scheduled windows.
	// Every zero field takes the built-in default.
	Backup BackupPolicy
}

// The RDS control plane's KV-backed handler set. One per daemon.
type Service struct {
	nc     *nats.Conn
	region string
	deps   Deps

	// Heartbeat state that never reaches KV: beats are counted here and
	// persisted only on change or on the slower floor.
	livenessMu sync.Mutex
	liveness   map[string]*agentLiveness

	// buckets memoises one Bucket per account, plus the single system bucket.
	// Resolution is per request, so without this every RDS call pays a bucket
	// create round trip before it does any work.
	bucketsMu sync.Mutex
	buckets   map[string]*kvstore.Bucket
	systemB   *kvstore.Bucket
}

type agentLiveness struct {
	lastSeen     time.Time
	health       EngineHealth
	message      string
	beatsSinceKV int
}

// region scopes the ARNs the Service mints.
func NewService(nc *nats.Conn, region string) *Service {
	return &Service{nc: nc, region: region, liveness: make(map[string]*agentLiveness)}
}

func (s *Service) WithDeps(d Deps) *Service {
	s.deps = d
	return s
}

func (s *Service) bootstrapTimeout() time.Duration {
	if s.deps.BootstrapTimeout > 0 {
		return s.deps.BootstrapTimeout
	}
	return defaultBootstrapTimeout
}

func (s *Service) vmStopTimeout() time.Duration {
	if s.deps.VMStopTimeout > 0 {
		return s.deps.VMStopTimeout
	}
	return defaultVMStopTimeout
}

func (s *Service) applyParamsTimeout() time.Duration {
	if s.deps.ApplyParamsTimeout > 0 {
		return s.deps.ApplyParamsTimeout
	}
	return defaultApplyParamsTimeout
}

func (s *Service) failureGrace() time.Duration {
	if s.deps.FailureGrace > 0 {
		return s.deps.FailureGrace
	}
	return defaultFailureGrace
}

func (s *Service) modifyLeaseTTL() time.Duration {
	if s.deps.ModifyLeaseTTL > 0 {
		return s.deps.ModifyLeaseTTL
	}
	return modifyLeaseTTL
}

func (s *Service) modifyLeaseRefresh() time.Duration {
	ttl := s.modifyLeaseTTL()
	if refresh := s.deps.ModifyLeaseRefresh; refresh > 0 && refresh < ttl {
		return refresh
	}
	if refresh := ttl / 3; refresh > 0 {
		return refresh
	}
	return time.Nanosecond
}

func (s *Service) js() (jetstream.JetStream, error) {
	if s.nc == nil {
		return nil, errors.New("rds service: nil nats connection")
	}
	return jetstream.New(s.nc)
}

// bucket resolves accountID's bucket, memoised: the resolution used to bill a
// stream create per RDS call, and a Bucket also carries the recovery a raw
// handle cannot — a lost stream is reopened under the next read rather than
// failing every call until the process restarts.
func (s *Service) bucket(ctx context.Context, accountID string) (*kvstore.Bucket, error) {
	js, err := s.js()
	if err != nil {
		return nil, err
	}
	s.bucketsMu.Lock()
	b, ok := s.buckets[accountID]
	if !ok {
		b = kvstore.NewBucket(js, AccountBucketConfig(accountID))
		if s.buckets == nil {
			s.buckets = make(map[string]*kvstore.Bucket)
		}
		s.buckets[accountID] = b
	}
	s.bucketsMu.Unlock()
	// Opened outside the lock, under the Bucket's own, so one account's slow
	// first open does not hold every other account's resolution behind it.
	if _, err := b.KV(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *Service) systemBucket(ctx context.Context) (*kvstore.Bucket, error) {
	js, err := s.js()
	if err != nil {
		return nil, err
	}
	s.bucketsMu.Lock()
	if s.systemB == nil {
		s.systemB = kvstore.NewBucket(js, SystemBucketConfig())
	}
	b := s.systemB
	s.bucketsMu.Unlock()
	if _, err := b.KV(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

// Both paths empty deliberately disables TLS; a partial configuration is an
// error rather than an accidental plaintext deployment.
func (s *Service) loadCA() (*x509.Certificate, *rsa.PrivateKey, error) {
	if s.deps.LoadCA != nil {
		return s.deps.LoadCA()
	}
	if s.deps.CACertPath == "" && s.deps.CAKeyPath == "" {
		return nil, nil, nil
	}
	if s.deps.CACertPath == "" || s.deps.CAKeyPath == "" {
		return nil, nil, errors.New("rds service: incomplete cluster CA configuration")
	}
	return admin.LoadCAKeyPair(s.deps.CACertPath, s.deps.CAKeyPath)
}

// Whether this deployment can serve TLS at all, which is whether it holds a
// cluster CA to mint a serving certificate from. A formed deployment always
// does, so this is an assertion rather than an operational switch.
func (s *Service) tlsAvailable() (bool, error) {
	caCert, caKey, err := s.loadCA()
	if err != nil {
		return false, err
	}
	return caCert != nil && caKey != nil, nil
}

// The eight helpers below are the account bucket's record layer. They are
// adapters over kvstore rather than a codec of their own: the JSON, the error
// wrapping and the stream recovery belong to kvstore, and what stays here is
// the not-found convention its callers are written against.

// Returns (false, nil) when the key is absent.
func getJSON[T any](ctx context.Context, b *kvstore.Bucket, key string, out *T) (bool, error) {
	v, _, err := kvstore.On[T](b).Get(ctx, key)
	if errors.Is(err, kvstore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	*out = *v
	return true, nil
}

// getJSON plus the entry revision, for callers that follow with a CAS update.
func getJSONRevision[T any](ctx context.Context, b *kvstore.Bucket, key string, out *T) (uint64, bool, error) {
	v, rev, err := kvstore.On[T](b).Get(ctx, key)
	if errors.Is(err, kvstore.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	*out = *v
	return rev, true, nil
}

// getJSONRevisionAny is getJSONRevision for the one caller whose record type is
// chosen at runtime from the ARN, so no type parameter can name it. The bytes
// still come through kvstore; only the decode is dynamic.
func getJSONRevisionAny(ctx context.Context, b *kvstore.Bucket, key string, out any) (uint64, bool, error) {
	raw, rev, err := kvstore.On[json.RawMessage](b).Get(ctx, key)
	if errors.Is(err, kvstore.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if err := json.Unmarshal(*raw, out); err != nil {
		return 0, false, fmt.Errorf("unmarshal %s: %w", key, err)
	}
	return rev, true, nil
}

func putJSON[T any](ctx context.Context, b *kvstore.Bucket, key string, v T) error {
	return kvstore.On[T](b).Set(ctx, key, &v)
}

// Writes v at key only if nothing is stored there, so the key doubles as a
// cluster-wide reservation. Returns kvstore.ErrExists when it is taken.
func createJSON[T any](ctx context.Context, b *kvstore.Bucket, key string, v T) error {
	_, err := createJSONRevision(ctx, b, key, v)
	return err
}

// createJSON plus the created entry's revision, for callers that must undo only
// the reservation they created rather than a concurrent replacement.
func createJSONRevision[T any](ctx context.Context, b *kvstore.Bucket, key string, v T) (uint64, error) {
	return kvstore.On[T](b).Create(ctx, key, &v)
}

// Writes v at key only if the stored entry is still at rev. Returns
// kvstore.ErrConflict when another writer got there first.
func updateJSON[T any](ctx context.Context, b *kvstore.Bucket, key string, rev uint64, v T) error {
	_, err := updateJSONRevision(ctx, b, key, rev, v)
	return err
}

// updateJSON plus the revision written by the successful CAS update.
func updateJSONRevision[T any](ctx context.Context, b *kvstore.Bucket, key string, rev uint64, v T) (uint64, error) {
	return kvstore.On[T](b).CompareAndSet(ctx, key, &v, rev)
}
