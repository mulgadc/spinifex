package gateway_ecr

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecr"
)

// DefaultLifecycleSweepInterval is the period between lifecycle expiry cycles.
const DefaultLifecycleSweepInterval = time.Hour

// LifecycleSweeper periodically applies each repository's stored lifecycle
// policy: it evaluates the shared rule engine against the repo's current images
// and deletes the expired set through the registry's GC reclaim path. It runs in
// the gateway because only the gateway holds a Registry (object store + meta);
// the daemon owns the KV but cannot reclaim predastore bytes.
//
// The sweep is idempotent and safe to run concurrently across awsgw instances:
// a delete of an already-gone image is a no-op, and per-item errors are logged
// and skipped so one bad repo never aborts the cycle.
type LifecycleSweeper struct {
	reg      *Registry
	accounts func() ([]string, error)
	interval time.Duration
	now      func() time.Time
}

// NewLifecycleSweeper builds a sweeper over reg. accounts enumerates the account
// IDs to sweep (typically ACTIVE IAM accounts). A zero interval falls back to the
// default.
func NewLifecycleSweeper(reg *Registry, accounts func() ([]string, error), interval time.Duration) *LifecycleSweeper {
	if interval <= 0 {
		interval = DefaultLifecycleSweepInterval
	}
	return &LifecycleSweeper{
		reg:      reg,
		accounts: accounts,
		interval: interval,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// Run sweeps on every tick until ctx is cancelled. Blocks until then.
func (s *LifecycleSweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	slog.InfoContext(ctx, "ECR lifecycle sweeper started", "interval", s.interval)
	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "ECR lifecycle sweeper stopped")
			return
		case <-ticker.C:
			s.sweepOnce(ctx, s.now())
		}
	}
}

// sweepOnce runs one cycle over every account/repo, returning the number of
// images expired. Per-account and per-repo errors are logged and skipped.
func (s *LifecycleSweeper) sweepOnce(ctx context.Context, now time.Time) int {
	accounts, err := s.accounts()
	if err != nil {
		slog.WarnContext(ctx, "ECR lifecycle sweep: list accounts failed", "err", err)
		return 0
	}

	var deleted int
	for _, account := range accounts {
		repos, err := s.reg.Meta.ListRepos(ctx, account)
		if err != nil {
			if errors.Is(err, ecr.ErrNotFound) {
				continue
			}
			slog.WarnContext(ctx, "ECR lifecycle sweep: list repos failed", "account", account, "err", err)
			continue
		}
		for _, repo := range repos {
			deleted += s.sweepRepo(ctx, account, repo, now)
		}
	}
	return deleted
}

// sweepRepo applies one repo's lifecycle policy. A repo with no policy is left
// untouched. Returns the count of images expired in this repo.
func (s *LifecycleSweeper) sweepRepo(ctx context.Context, account, repo string, now time.Time) int {
	policy, err := s.reg.Meta.GetLifecyclePolicy(ctx, account, repo)
	if err != nil {
		if !errors.Is(err, ecr.ErrNotFound) {
			slog.WarnContext(ctx, "ECR lifecycle sweep: get policy failed", "account", account, "repo", repo, "err", err)
		}
		return 0
	}
	if len(policy) == 0 {
		return 0
	}

	records, err := s.reg.ListImages(ctx, account, repo)
	if err != nil {
		if !errors.Is(err, ecr.ErrNotFound) {
			slog.WarnContext(ctx, "ECR lifecycle sweep: list images failed", "account", account, "repo", repo, "err", err)
		}
		return 0
	}

	images := make([]ecr.LifecycleImage, 0, len(records))
	for _, rec := range records {
		images = append(images, ecr.LifecycleImage{Digest: rec.Digest, Tags: rec.Tags, PushedAt: rec.PushedAt})
	}

	expiries, err := ecr.EvaluateLifecyclePolicy(policy, images, now)
	if err != nil {
		slog.WarnContext(ctx, "ECR lifecycle sweep: evaluate policy failed", "account", account, "repo", repo, "err", err)
		return 0
	}
	if len(expiries) == 0 {
		return 0
	}

	// Optimistic-concurrency guard: re-read the current tag set immediately
	// before deleting. If an image gained or changed tags since evaluation, a
	// concurrent push/re-tag landed — skip it and let the next cycle re-evaluate
	// the new state. The cross-repo blob mount guard is provided separately by
	// reclaimManifest/referencedDigests.
	fresh, err := s.reg.ListImages(ctx, account, repo)
	if err != nil {
		slog.WarnContext(ctx, "ECR lifecycle sweep: re-list images failed", "account", account, "repo", repo, "err", err)
		return 0
	}
	currentTags := make(map[string][]string, len(fresh))
	present := make(map[string]bool, len(fresh))
	for _, rec := range fresh {
		currentTags[rec.Digest] = rec.Tags
		present[rec.Digest] = true
	}

	var deleted int
	for _, exp := range expiries {
		if !present[exp.Digest] {
			continue // already gone
		}
		if !sameTagSet(exp.Tags, currentTags[exp.Digest]) {
			slog.InfoContext(ctx, "ECR lifecycle sweep: skipping image changed since evaluation",
				"account", account, "repo", repo, "digest", exp.Digest)
			continue
		}
		if _, err := s.reg.DeleteImage(ctx, account, repo, "", exp.Digest); err != nil {
			if errors.Is(err, ErrImageNotFound) {
				continue
			}
			slog.WarnContext(ctx, "ECR lifecycle sweep: delete image failed",
				"account", account, "repo", repo, "digest", exp.Digest, "err", err)
			continue
		}
		slog.InfoContext(ctx, "ECR lifecycle sweep: expired image",
			"account", account, "repo", repo, "digest", exp.Digest,
			"tags", exp.Tags, "rulePriority", exp.RulePriority)
		deleted++
	}
	return deleted
}

// sameTagSet reports whether two tag slices hold the same set of tags,
// order-independent.
func sameTagSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, t := range a {
		seen[t]++
	}
	for _, t := range b {
		if seen[t] == 0 {
			return false
		}
		seen[t]--
	}
	return true
}
