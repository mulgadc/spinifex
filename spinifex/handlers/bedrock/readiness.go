package handlers_bedrock

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// defaultStartupTimeout bounds how long Ensure's launch goroutine polls a
// freshly-launched serving VM's /v1/models endpoint before giving up and
// reverting the record to ABSENT.
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

// readinessPollInterval is the spacing between /v1/models probes. Short
// enough that a fast-booting VM isn't held back by the poll cadence, long
// enough not to hammer a guest still bringing its network stack up.
const readinessPollInterval = 2 * time.Second

// waitReady polls baseURL + "/v1/models" every pollInterval until it returns
// HTTP 200 or ctx's deadline (set by the caller from startupTimeout) expires.
// Never a sleep in place of a real check: every non-200 (including connection
// refused, while the guest is still booting) is swallowed and retried, since
// readiness is the very thing being waited for. pollInterval is a parameter
// (not just the readinessPollInterval constant) so tests can poll fast
// without waiting out the production cadence.
func waitReady(ctx context.Context, client *http.Client, baseURL string, pollInterval time.Duration) error {
	url := baseURL + "/v1/models"
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

// waitReadyAll polls every baseURL in baseURLs (keyed by model id) in
// parallel until each answers ready or ctx's deadline expires, joining every
// member's failure so a caller learns exactly which member of the bundle
// never came up rather than just "readiness failed".
func waitReadyAll(ctx context.Context, client *http.Client, baseURLs map[string]string, pollInterval time.Duration) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	wg.Add(len(baseURLs))
	for modelID, baseURL := range baseURLs {
		go func(modelID, baseURL string) {
			defer wg.Done()
			if err := waitReady(ctx, client, baseURL, pollInterval); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", modelID, err))
				mu.Unlock()
			}
		}(modelID, baseURL)
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
