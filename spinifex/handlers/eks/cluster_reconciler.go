package handlers_eks

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	defaultReconcileLeaseRefresh = 20 * time.Second
	defaultReconcileInterval     = 30 * time.Second
	defaultHealthzTimeout        = 5 * time.Second
	// defaultCreateTimeout bounds how long a cluster may sit in CREATING before
	// the reconciler gives up and marks it FAILED. Measured from meta.CreatedAt
	// (absolute), so a VM that never boots reaches a terminal state even across
	// daemon restarts. Plan Risk #1.
	defaultCreateTimeout = 15 * time.Minute
)

// ErrReconcilerLeaseLost is returned by Run when the leader lease was lost
// mid-loop. Callers can re-Acquire and re-Run if they want to keep driving
// reconcile from the same node.
var ErrReconcilerLeaseLost = errors.New("eks: reconciler lease lost")

// ErrReconcilerClusterDeleting is returned by Run when meta.Status is
// observed as DELETING; the DeleteCluster service-impl path is responsible
// for teardown, so the reconciler exits without further work.
var ErrReconcilerClusterDeleting = errors.New("eks: cluster deleting")

// ErrReconcilerClusterFailed is returned by Run when meta.Status is FAILED.
var ErrReconcilerClusterFailed = errors.New("eks: cluster failed")

// HTTPDoer is the minimum http.Client surface the reconciler needs for
// /healthz probes; tests stub it without a TLS server.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// ClusterReconciler drives one cluster's CREATING → ACTIVE → DELETING
// lifecycle under a leader lease. One instance per (accountID, clusterName);
// the leader lease arbitrates which spinifex node actually drives reconcile.
type ClusterReconciler struct {
	leaderKV    nats.KeyValue
	acctKV      nats.KeyValue
	accountID   string
	clusterName string
	holderID    string
	healthURL   string

	leaseRefresh  time.Duration
	interval      time.Duration
	healthTimeout time.Duration
	createTimeout time.Duration
	httpClient    HTTPDoer
}

// ReconcilerOption tunes a ClusterReconciler. Tests use the With* helpers to
// shrink tickers and inject a stub HTTP client.
type ReconcilerOption func(*ClusterReconciler)

// WithReconcileInterval overrides the default reconcile loop period.
func WithReconcileInterval(d time.Duration) ReconcilerOption {
	return func(r *ClusterReconciler) { r.interval = d }
}

// WithLeaseRefresh overrides the default leader-lease refresh period. Should
// be less than KVBucketEKSLeaderTTL.
func WithLeaseRefresh(d time.Duration) ReconcilerOption {
	return func(r *ClusterReconciler) { r.leaseRefresh = d }
}

// WithHealthzTimeout overrides the per-probe HTTP timeout.
func WithHealthzTimeout(d time.Duration) ReconcilerOption {
	return func(r *ClusterReconciler) { r.healthTimeout = d }
}

// WithCreateTimeout overrides how long a cluster may remain CREATING before the
// reconciler marks it FAILED.
func WithCreateTimeout(d time.Duration) ReconcilerOption {
	return func(r *ClusterReconciler) { r.createTimeout = d }
}

// WithHTTPClient injects a stub HTTPDoer (tests).
func WithHTTPClient(c HTTPDoer) ReconcilerOption {
	return func(r *ClusterReconciler) { r.httpClient = c }
}

// NewClusterReconciler constructs a ClusterReconciler. healthURL is the
// fully-qualified probe target (e.g. https://nlb-dns:443/healthz) or empty
// to skip /healthz probing (CREATING transitions on bootstrap state alone).
func NewClusterReconciler(leaderKV, acctKV nats.KeyValue, accountID, clusterName, holderID, healthURL string, opts ...ReconcilerOption) (*ClusterReconciler, error) {
	if leaderKV == nil {
		return nil, errors.New("eks: NewClusterReconciler nil leaderKV")
	}
	if acctKV == nil {
		return nil, errors.New("eks: NewClusterReconciler nil acctKV")
	}
	if accountID == "" {
		return nil, errors.New("eks: NewClusterReconciler empty accountID")
	}
	if clusterName == "" {
		return nil, errors.New("eks: NewClusterReconciler empty clusterName")
	}
	if holderID == "" {
		return nil, errors.New("eks: NewClusterReconciler empty holderID")
	}
	r := &ClusterReconciler{
		leaderKV:      leaderKV,
		acctKV:        acctKV,
		accountID:     accountID,
		clusterName:   clusterName,
		holderID:      holderID,
		healthURL:     healthURL,
		leaseRefresh:  defaultReconcileLeaseRefresh,
		interval:      defaultReconcileInterval,
		healthTimeout: defaultHealthzTimeout,
		createTimeout: defaultCreateTimeout,
		httpClient: &http.Client{
			Timeout: defaultHealthzTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, //nolint:gosec // K3s NLB is self-signed; CA pinning is a v2 hardening item
				},
			},
		},
	}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

func reconcilerLeaderKey(accountID, clusterName string) string {
	return accountID + "/" + clusterName
}

// AcquireLease attempts a CAS Create on the leader bucket. Returns
// (release, true) if this caller is now the leader; (nil, false) otherwise.
// The release closure performs a best-effort kv.Delete; the bucket's 60s
// TTL reaps stale leases when release does not run (crashed leader).
func (r *ClusterReconciler) AcquireLease() (func(), bool) {
	key := reconcilerLeaderKey(r.accountID, r.clusterName)
	if _, err := r.leaderKV.Create(key, []byte(r.holderID)); err != nil {
		slog.Debug("ClusterReconciler: lease held by another holder",
			"key", key, "holderID", r.holderID, "err", err)
		return nil, false
	}
	slog.Info("ClusterReconciler: lease acquired", "key", key, "holderID", r.holderID)
	return func() {
		if err := r.leaderKV.Delete(key); err != nil {
			slog.Warn("ClusterReconciler: lease release failed (TTL will reap)",
				"key", key, "holderID", r.holderID, "err", err)
		}
	}, true
}

// RefreshLease re-asserts ownership via a CAS update on the leader key.
// Returns false if the key was taken by another holder or the CAS conflicts.
func (r *ClusterReconciler) RefreshLease() bool {
	key := reconcilerLeaderKey(r.accountID, r.clusterName)
	entry, err := r.leaderKV.Get(key)
	if err != nil {
		slog.Warn("ClusterReconciler: lease get failed", "key", key, "err", err)
		return false
	}
	if string(entry.Value()) != r.holderID {
		slog.Info("ClusterReconciler: lease now held by another holder",
			"key", key, "holderID", r.holderID, "got", string(entry.Value()))
		return false
	}
	if _, err := r.leaderKV.Update(key, []byte(r.holderID), entry.Revision()); err != nil {
		slog.Warn("ClusterReconciler: lease CAS update failed",
			"key", key, "holderID", r.holderID, "err", err)
		return false
	}
	return true
}

// Run drives reconcile until ctx is cancelled, the lease is lost, or the
// cluster reaches a terminal state. Caller is expected to AcquireLease first;
// Run does not re-acquire.
func (r *ClusterReconciler) Run(ctx context.Context) error {
	refreshT := time.NewTicker(r.leaseRefresh)
	defer refreshT.Stop()
	reconcileT := time.NewTicker(r.interval)
	defer reconcileT.Stop()

	if err := r.reconcileOnce(ctx); err != nil {
		if terminalReconcileErr(err) {
			return err
		}
		slog.Warn("ClusterReconciler: initial reconcile failed",
			"cluster", r.clusterName, "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-refreshT.C:
			if !r.RefreshLease() {
				return ErrReconcilerLeaseLost
			}
		case <-reconcileT.C:
			if err := r.reconcileOnce(ctx); err != nil {
				if terminalReconcileErr(err) {
					return err
				}
				slog.Warn("ClusterReconciler: reconcile failed",
					"cluster", r.clusterName, "err", err)
			}
		}
	}
}

func terminalReconcileErr(err error) bool {
	return errors.Is(err, ErrReconcilerClusterDeleting) ||
		errors.Is(err, ErrReconcilerClusterFailed) ||
		errors.Is(err, ErrClusterNotFound)
}

func (r *ClusterReconciler) reconcileOnce(ctx context.Context) error {
	meta, err := GetClusterMeta(r.acctKV, r.clusterName)
	if err != nil {
		return err
	}
	switch meta.Status {
	case ClusterStatusCreating:
		ready, reason := r.bootstrapReady(meta)
		if !ready {
			slog.Debug("ClusterReconciler: bootstrap not ready",
				"cluster", r.clusterName, "reason", reason)
			return r.failIfCreateTimedOut(meta, "bootstrap not ready: "+reason)
		}
		if err := r.probeHealthz(ctx); err != nil {
			slog.Debug("ClusterReconciler: /healthz still failing",
				"cluster", r.clusterName, "err", err)
			return r.failIfCreateTimedOut(meta, "healthz not ready: "+err.Error())
		}
		if err := SetClusterStatus(r.acctKV, r.clusterName, ClusterStatusActive); err != nil {
			return err
		}
		slog.Info("ClusterReconciler: cluster transitioned to ACTIVE",
			"cluster", r.clusterName)
	case ClusterStatusActive:
		if err := r.probeHealthz(ctx); err != nil {
			slog.Warn("ClusterReconciler: /healthz failing for ACTIVE cluster",
				"cluster", r.clusterName, "err", err)
		}
	case ClusterStatusDeleting:
		return ErrReconcilerClusterDeleting
	case ClusterStatusFailed:
		return ErrReconcilerClusterFailed
	}
	return nil
}

// failIfCreateTimedOut transitions a still-CREATING cluster to FAILED once it
// has exceeded createTimeout since meta.CreatedAt, and returns
// ErrReconcilerClusterFailed so Run exits the loop. While still within the
// window it returns nil so the next tick retries. A zero or absent CreatedAt
// disables the deadline (no false-positive fail on a malformed record).
func (r *ClusterReconciler) failIfCreateTimedOut(meta *ClusterMeta, reason string) error {
	if r.createTimeout <= 0 || meta.CreatedAt.IsZero() {
		return nil
	}
	if time.Since(meta.CreatedAt) < r.createTimeout {
		return nil
	}
	failReason := fmt.Sprintf("cluster did not become ACTIVE within %s: %s", r.createTimeout, reason)
	if err := MarkClusterFailed(r.acctKV, r.clusterName, failReason); err != nil {
		if errors.Is(err, ErrClusterNotFound) {
			return ErrClusterNotFound
		}
		return fmt.Errorf("mark create-timeout failed: %w", err)
	}
	slog.Warn("ClusterReconciler: CREATING timed out, marked FAILED",
		"cluster", r.clusterName, "timeout", r.createTimeout, "reason", reason)
	return ErrReconcilerClusterFailed
}

// bootstrapReady returns true once the four NATS bootstrap KV artifacts are
// present (node token, admin kubeconfig, OIDC JWKS, and the CA on meta).
func (r *ClusterReconciler) bootstrapReady(meta *ClusterMeta) (bool, string) {
	if meta.CertificateAuthorityB64 == "" {
		return false, "CA absent on meta"
	}
	keys := []string{
		NodeTokenKey(r.clusterName),
		AdminKubeconfigKey(r.clusterName),
		OIDCJWKSKey(r.clusterName),
	}
	for _, k := range keys {
		if _, err := r.acctKV.Get(k); err != nil {
			return false, fmt.Sprintf("%s missing: %s", k, err)
		}
	}
	return true, ""
}

func (r *ClusterReconciler) probeHealthz(ctx context.Context) error {
	if r.healthURL == "" {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, r.healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, r.healthURL, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthz request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("eks: /healthz status %d", resp.StatusCode)
	}
	return nil
}
