package workerboot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
	"github.com/nats-io/nkeys"
)

// Store is the Region's store launch as the gateway reports it. Bound means a
// VM was launched for the store, not that it serves.
type Store struct {
	Bound        bool
	Address      string
	PublicKey    string
	ServerKeyPin string
	HostNodes    int
}

// StoreDescriber asks the gateway for this worker's store.
type StoreDescriber interface {
	DescribeStore(ctx context.Context) (Store, error)
}

// NewDescriber builds the gateway client the store's address comes from.
type NewDescriber func(region, gatewayURL string) (StoreDescriber, error)

// Checker proves the store's JetStream answers this worker's key at address.
// Only an error with Retryable() true is asked again.
type Checker interface {
	Check(ctx context.Context, address string, kp nkeys.KeyPair) error
}

// ReadyConfig is Config plus the readiness capabilities and cadence.
type ReadyConfig struct {
	Config

	NewDescriber NewDescriber
	Checker      Checker
	// CallTimeout bounds one describe-and-check pass; zero is 30 seconds.
	CallTimeout time.Duration
	// Refresh is the wait after a pass before checking again; zero is 30s.
	Refresh time.Duration
	// Refused is the wait after a failure that cannot clear by itself, such
	// as a refused or mismatched key; zero is five minutes.
	Refused time.Duration
	// Serve, when set, also serves SQS owner calls while Watch runs.
	Serve *ServeConfig
}

// ErrStoreKeyMismatch is a bound store launched for a key other than the one
// on this worker's disk; it is asked again only at the Refused cadence.
var ErrStoreKeyMismatch = errors.New("the store was launched for a different worker key")

const readyFile = "store-ready"

// Watch keeps the readiness marker true until ctx ends: it re-proves the disk,
// then checks the store with the worker's existing key every pass, writing the
// marker on a pass and removing it on any failure and on return. It never
// creates a key. NKey authenticates this client only, not the store server.
func Watch(ctx context.Context, cfg ReadyConfig) error {
	marker := filepath.Join(cfg.RunDir, readyFile)
	defer func() { _ = os.Remove(marker) }()
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear stale store readiness: %w", err)
	}
	c, err := readContract(cfg.FwCfgDir)
	if err != nil {
		return err
	}
	if err := mountWorkerDisk(cfg.Config, c.serial); err != nil {
		return err
	}
	if cfg.NewDescriber == nil || cfg.Checker == nil {
		return errors.New("no gateway client or store checker is configured")
	}
	d, err := cfg.NewDescriber(c.region, c.gatewayURL)
	if err != nil {
		return fmt.Errorf("build gateway client: %w", err)
	}
	keys := workerkey.Store{Dir: filepath.Join(cfg.DataDir, keyDirName)}
	if cfg.Serve != nil {
		done := make(chan struct{})
		go func() {
			defer close(done)
			serve(ctx, cfg, c, d, keys)
		}()
		// Calls in flight finish before the marker is removed and Watch returns.
		defer func() { <-done }()
	}
	callTimeout := orDefault(cfg.CallTimeout, 30*time.Second)
	refresh := orDefault(cfg.Refresh, 30*time.Second)
	refused := orDefault(cfg.Refused, 5*time.Minute)

	last := ""
	for {
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		st, err := readyOnce(callCtx, cfg, d, keys)
		cancel()
		delay, state := refresh, ""
		if err == nil {
			err = writeReady(cfg.RunDir, st)
		}
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil:
			state = fmt.Sprintf("store at %s answered the worker key (client-authenticated only; the store server is not authenticated)", st.Address)
		default:
			_ = os.Remove(marker)
			delay = cfg.Wait
			if !pending(err) && !retryable(err) && !errors.Is(err, errStorePending) {
				delay = refused
			}
			state = fmt.Sprintf("store not ready: %v", err)
		}
		// Only a change is logged, so a long wait does not flood the console.
		if state != last {
			cfg.Log(state)
			last = state
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(max(delay, time.Millisecond)):
		}
	}
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

var errStorePending = errors.New("the store is not yet bound")

func readyOnce(ctx context.Context, cfg ReadyConfig, d StoreDescriber, keys workerkey.Store) (Store, error) {
	st, err := d.DescribeStore(ctx)
	if err != nil {
		return Store{}, err
	}
	if !st.Bound {
		return Store{}, errStorePending
	}
	kp, err := keys.ForExistingStore(st.PublicKey)
	if err != nil {
		if errors.Is(err, workerkey.ErrKeyMismatch) {
			return Store{}, fmt.Errorf("%w: %w", ErrStoreKeyMismatch, err)
		}
		return Store{}, err
	}
	defer kp.Wipe()
	if err := cfg.Checker.Check(ctx, st.Address, kp); err != nil {
		return Store{}, err
	}
	return st, nil
}

// writeReady publishes only the address and public key, never the seed.
func writeReady(runDir string, st Store) error {
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", runDir, err)
	}
	body := "address=" + st.Address + "\npublic=" + st.PublicKey + "\n"
	if err := os.WriteFile(filepath.Join(runDir, readyFile), []byte(body), 0o600); err != nil {
		return fmt.Errorf("publish store readiness: %w", err)
	}
	return nil
}
