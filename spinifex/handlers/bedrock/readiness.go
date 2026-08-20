package handlers_bedrock

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	gateway_bedrock "github.com/mulgadc/spinifex/spinifex/gateway/bedrock"
)

// defaultStartupTimeout bounds how long Ensure's launch goroutine polls a
// freshly-launched serving VM's per-member readiness route before giving up
// and reverting the record to ABSENT.
//
// Measured once, on the only GPU available to test on: Llama 3.2 1B on an
// RTX A1000 reached "Application startup complete" 257.6s after launch, of
// which 20.7s was guest boot and 26.3s a cold torch.compile. The former 5min
// left ~42s of margin on the smallest model this platform can serve, so the
// bound is set well clear of the one data point rather than close to it. A
// larger card and a larger model are unmeasured, and the failure mode of too
// low a value is a healthy endpoint torn down mid-start; too high only costs
// a stuck launch more time to give up.
const defaultStartupTimeout = 15 * time.Minute

// readinessPollInterval is the spacing between readiness probes. Short
// enough that a fast-booting VM isn't held back by the poll cadence, long
// enough not to hammer a guest still bringing its network stack up.
const readinessPollInterval = 2 * time.Second

// readinessPath returns the route a member's engine answers readiness on:
// vLLM (familyMeta) serves OpenAI /v1/models; TEI has no such route, so every
// other family is probed on /health. Mirrors engineForFamily's selection.
func readinessPath(family string) string {
	if family == gateway_bedrock.FamilyMeta {
		return "/v1/models"
	}
	return "/health"
}

// readinessTarget is one bundle member's own base address and the path its
// engine answers readiness on.
type readinessTarget struct {
	BaseURL string
	Path    string
}

// waitReady polls target.BaseURL+target.Path every pollInterval until it
// returns HTTP 200 or ctx's deadline expires; every non-200 (including a
// refused connection while the guest boots) is swallowed and retried.
func waitReady(ctx context.Context, client *http.Client, target readinessTarget, pollInterval time.Duration) error {
	url := target.BaseURL + target.Path
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		if probeOnce(ctx, client, url) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("bedrock: readiness probe of %s: %w", url, ctx.Err())
		case <-ticker.C:
		}
	}
}

// waitReadyAll polls every target (keyed by model id) in parallel, each on
// its own engine's route, until all answer ready or ctx expires; member
// failures are joined so the caller learns which member never came up.
func waitReadyAll(ctx context.Context, client *http.Client, targets map[string]readinessTarget, pollInterval time.Duration) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	wg.Add(len(targets))
	for modelID, target := range targets {
		go func(modelID string, target readinessTarget) {
			defer wg.Done()
			if err := waitReady(ctx, client, target, pollInterval); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", modelID, err))
				mu.Unlock()
			}
		}(modelID, target)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// probeOnce issues a single GET and reports whether it returned HTTP 200.
// Any transport error (guest not listening yet) or non-200 status counts as
// not-yet-ready, not a failure — the caller's context deadline is what
// eventually turns "not yet" into an error.
func probeOnce(ctx context.Context, client *http.Client, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
