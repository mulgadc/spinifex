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
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Deps are the host-side capabilities the RDS control plane needs beyond NATS.
type Deps struct {
	// CACertPath and CAKeyPath locate the cluster CA the serving certs are
	// signed by. Empty disables minting, and GetDBBootstrapConfig then returns
	// no cert rather than failing the boot.
	CACertPath string
	CAKeyPath  string
	// LoadCA overrides the file-backed loader in tests.
	LoadCA CALoader
	// Launch bundles the EC2-family collaborators a DB VM is composed from.
	Launch LaunchDeps
}

// Service is the RDS control plane's KV-backed handler set. One per daemon.
type Service struct {
	nc     *nats.Conn
	region string
	deps   Deps

	// liveness holds the heartbeat state that never reaches KV. Beats are
	// counted here and persisted only on change or on the slower floor, so a
	// steady fleet does not write KV twice a minute per instance.
	livenessMu sync.Mutex
	liveness   map[string]*agentLiveness
}

// agentLiveness is one instance's unpersisted beat state.
type agentLiveness struct {
	lastSeen     time.Time
	health       EngineHealth
	message      string
	beatsSinceKV int
}

// NewService constructs a Service bound to a NATS connection. region scopes the
// ARNs it mints.
func NewService(nc *nats.Conn, region string) *Service {
	return &Service{nc: nc, region: region, liveness: make(map[string]*agentLiveness)}
}

// WithDeps attaches host capabilities and returns the Service for chaining.
func (s *Service) WithDeps(d Deps) *Service {
	s.deps = d
	return s
}

func (s *Service) js() (jetstream.JetStream, error) {
	if s.nc == nil {
		return nil, errors.New("rds service: nil nats connection")
	}
	return jetstream.New(s.nc)
}

// bucket returns the per-account KV handle, creating it on first use.
func (s *Service) bucket(ctx context.Context, accountID string) (jetstream.KeyValue, error) {
	js, err := s.js()
	if err != nil {
		return nil, err
	}
	return GetOrCreateAccountBucket(ctx, js, accountID)
}

// systemBucket returns the shared rds-system KV handle holding the reverse index.
func (s *Service) systemBucket(ctx context.Context) (jetstream.KeyValue, error) {
	js, err := s.js()
	if err != nil {
		return nil, err
	}
	return GetOrCreateSystemBucket(ctx, js)
}

// loadCA resolves the cluster CA keypair, preferring an injected loader. Both
// empty paths deliberately disable TLS; a partial configuration is an error
// rather than an accidental plaintext deployment.
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

// getJSON reads key into out. Returns (false, nil) when the key is absent.
func getJSON(ctx context.Context, kv jetstream.KeyValue, key string, out any) (bool, error) {
	entry, err := kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(entry.Value(), out); err != nil {
		return false, fmt.Errorf("unmarshal %s: %w", key, err)
	}
	return true, nil
}

// getJSONRevision is getJSON plus the entry revision, for callers that follow
// the read with a CAS update.
func getJSONRevision(ctx context.Context, kv jetstream.KeyValue, key string, out any) (uint64, bool, error) {
	entry, err := kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if err := json.Unmarshal(entry.Value(), out); err != nil {
		return 0, false, fmt.Errorf("unmarshal %s: %w", key, err)
	}
	return entry.Revision(), true, nil
}

// putJSON marshals v and writes it at key.
func putJSON(ctx context.Context, kv jetstream.KeyValue, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// updateJSON writes v at key only if the stored entry is still at rev.
func updateJSON(ctx context.Context, kv jetstream.KeyValue, key string, rev uint64, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := kv.Update(ctx, key, data, rev); err != nil {
		return fmt.Errorf("update %s: %w", key, err)
	}
	return nil
}
