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
	Bound     bool
	Address   string
	PublicKey string
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

// ReadyConfig is Config plus the readiness capabilities.
type ReadyConfig struct {
	Config

	NewDescriber NewDescriber
	Checker      Checker
}

// ErrStoreKeyMismatch is a bound store launched for a key other than the one
// on this worker's disk; it is never retried.
var ErrStoreKeyMismatch = errors.New("the store was launched for a different worker key")

const readyFile = "store-ready"

// Ready proves the store answers this worker's key: it re-proves the worker
// disk is mounted, loads the existing key for the store's recorded public key,
// and checks the store's JetStream with it. It never creates a key. NKey
// authenticates this client only; the store server itself is not.
func Ready(ctx context.Context, cfg ReadyConfig) (Store, error) {
	c, err := readContract(cfg.FwCfgDir)
	if err != nil {
		return Store{}, err
	}
	if err := mountWorkerDisk(cfg.Config, c.serial); err != nil {
		return Store{}, err
	}
	if cfg.NewDescriber == nil || cfg.Checker == nil {
		return Store{}, errors.New("no gateway client or store checker is configured")
	}
	d, err := cfg.NewDescriber(c.region, c.gatewayURL)
	if err != nil {
		return Store{}, fmt.Errorf("build gateway client: %w", err)
	}
	keys := workerkey.Store{Dir: filepath.Join(cfg.DataDir, keyDirName)}

	attempts := max(cfg.Attempts, 1)
	for i := range attempts {
		st, err := readyOnce(ctx, cfg, d, keys)
		if err == nil {
			if err := writeReady(cfg.RunDir, st); err != nil {
				return Store{}, err
			}
			cfg.Log(fmt.Sprintf("store at %s answered the worker key (client-authenticated only; the store server is not authenticated)", st.Address))
			return st, nil
		}
		if i == attempts-1 || !pending(err) && !retryable(err) && !errors.Is(err, errStorePending) {
			return Store{}, err
		}
		cfg.Log(fmt.Sprintf("store not ready, asking again: %v", err))
		select {
		case <-ctx.Done():
			return Store{}, ctx.Err()
		case <-time.After(cfg.Wait):
		}
	}
	return Store{}, errors.New("store readiness attempts exhausted")
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
