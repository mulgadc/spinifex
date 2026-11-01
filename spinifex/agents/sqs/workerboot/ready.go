package workerboot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
)

// Checker proves the guest's local JetStream answers. Only an error with
// Retryable() true is asked again.
type Checker interface {
	Check(ctx context.Context) error
}

// ReadyConfig is Config plus the readiness capabilities and cadence.
type ReadyConfig struct {
	Config

	Checker Checker
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

const readyFile = "store-ready"

// Watch keeps the readiness marker true until ctx ends: it re-proves the disk,
// then checks the guest's local store every pass, writing the marker on a
// pass and removing it on any failure and on return. It never creates a key.
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
	if cfg.Checker == nil {
		return errNoChecker
	}
	keys := workerkey.Store{Dir: filepath.Join(cfg.DataDir, keyDirName)}
	if cfg.Serve != nil {
		done := make(chan struct{})
		go func() {
			defer close(done)
			serve(ctx, cfg, c, keys)
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
		err := cfg.Checker.Check(callCtx)
		cancel()
		delay, state := refresh, ""
		if err == nil {
			err = writeReady(cfg.RunDir)
		}
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil:
			state = "local SQS store is serving JetStream"
		default:
			_ = os.Remove(marker)
			delay = cfg.Wait
			if !pending(err) && !retryable(err) {
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

// writeReady publishes that the local store answered; it holds no secret.
func writeReady(runDir string) error {
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", runDir, err)
	}
	if err := os.WriteFile(filepath.Join(runDir, readyFile), []byte("ready\n"), 0o600); err != nil {
		return fmt.Errorf("publish store readiness: %w", err)
	}
	return nil
}
