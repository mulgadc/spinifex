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

// Host-side capabilities the RDS control plane needs beyond NATS.
type Deps struct {
	// The cluster CA the serving certs are signed by. Empty disables minting,
	// and GetDBBootstrapConfig returns no cert rather than failing the boot.
	CACertPath string
	CAKeyPath  string
	// Overrides the file-backed CA loader in tests.
	LoadCA CALoader
	Launch LaunchDeps
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

func (s *Service) js() (jetstream.JetStream, error) {
	if s.nc == nil {
		return nil, errors.New("rds service: nil nats connection")
	}
	return jetstream.New(s.nc)
}

func (s *Service) bucket(ctx context.Context, accountID string) (jetstream.KeyValue, error) {
	js, err := s.js()
	if err != nil {
		return nil, err
	}
	return GetOrCreateAccountBucket(ctx, js, accountID)
}

func (s *Service) systemBucket(ctx context.Context) (jetstream.KeyValue, error) {
	js, err := s.js()
	if err != nil {
		return nil, err
	}
	return GetOrCreateSystemBucket(ctx, js)
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

// Returns (false, nil) when the key is absent.
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

// getJSON plus the entry revision, for callers that follow with a CAS update.
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

// Writes v at key only if the stored entry is still at rev.
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
