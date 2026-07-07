package gateway_ecr

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sweepMediaType = "application/vnd.docker.distribution.manifest.v2+json"

// seedTimedImage writes a repo + manifest meta (with a controllable pushedAt) + tags
// directly into the registry's metadata, bypassing blob upload. Sufficient for
// exercising selection + DeleteImage; predastore reclaim is best-effort.
func seedTimedImage(t *testing.T, reg *Registry, account, repo, digest string, pushedAt time.Time, tags ...string) {
	t.Helper()
	require.NoError(t, reg.Meta.PutRepo(context.Background(), account, ecr.RepoMeta{Name: repo, CreatedAt: pushedAt}))
	require.NoError(t, reg.Meta.PutManifestMeta(context.Background(), account, repo, ecr.ManifestMeta{
		Digest: digest, MediaType: sweepMediaType, Size: 1, PushedAt: pushedAt,
	}))
	for _, tag := range tags {
		require.NoError(t, reg.Meta.PutTag(context.Background(), account, repo, tag, digest))
	}
}

func digestsOf(t *testing.T, reg *Registry, account, repo string) []string {
	t.Helper()
	records, err := reg.ListImages(context.Background(), account, repo)
	require.NoError(t, err)
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.Digest)
	}
	return out
}

func newSweeper(reg *Registry, accounts ...string) *LifecycleSweeper {
	return NewLifecycleSweeper(reg, func() ([]string, error) { return accounts, nil }, time.Hour)
}

func putPolicy(t *testing.T, reg *Registry, account, repo, policy string) {
	t.Helper()
	require.NoError(t, reg.Meta.PutLifecyclePolicy(context.Background(), account, repo, []byte(policy)))
}

const untaggedExpirePolicy = `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"untagged","countType":"sinceImagePushed","countUnit":"days","countNumber":7},"action":{"type":"expire"}}]}`

func TestSweepRepo_SinceImagePushed(t *testing.T) {
	reg := newTestRegistry()
	now := time.Now().UTC()
	repo := "team/app"
	seedTimedImage(t, reg, testAccount, repo, "sha256:olduntagged", now.AddDate(0, 0, -10))
	seedTimedImage(t, reg, testAccount, repo, "sha256:newuntagged", now.AddDate(0, 0, -2))
	seedTimedImage(t, reg, testAccount, repo, "sha256:oldtagged", now.AddDate(0, 0, -10), "v1")
	putPolicy(t, reg, testAccount, repo, untaggedExpirePolicy)

	deleted := newSweeper(reg, testAccount).sweepRepo(context.Background(), testAccount, repo, now)
	assert.Equal(t, 1, deleted)
	assert.ElementsMatch(t, []string{"sha256:newuntagged", "sha256:oldtagged"},
		digestsOf(t, reg, testAccount, repo))
}

func TestSweepRepo_ImageCountMoreThan(t *testing.T) {
	reg := newTestRegistry()
	now := time.Now().UTC()
	repo := "team/app"
	seedTimedImage(t, reg, testAccount, repo, "sha256:a", now.AddDate(0, 0, -1), "a")
	seedTimedImage(t, reg, testAccount, repo, "sha256:b", now.AddDate(0, 0, -2), "b")
	seedTimedImage(t, reg, testAccount, repo, "sha256:c", now.AddDate(0, 0, -3), "c")
	putPolicy(t, reg, testAccount, repo,
		`{"rules":[{"rulePriority":1,"selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":1},"action":{"type":"expire"}}]}`)

	deleted := newSweeper(reg, testAccount).sweepRepo(context.Background(), testAccount, repo, now)
	assert.Equal(t, 2, deleted)
	// Newest (a) kept; the two older expire.
	assert.Equal(t, []string{"sha256:a"}, digestsOf(t, reg, testAccount, repo))
}

func TestSweepRepo_NoPolicy(t *testing.T) {
	reg := newTestRegistry()
	now := time.Now().UTC()
	repo := "team/app"
	seedTimedImage(t, reg, testAccount, repo, "sha256:x", now.AddDate(0, 0, -100))

	deleted := newSweeper(reg, testAccount).sweepRepo(context.Background(), testAccount, repo, now)
	assert.Equal(t, 0, deleted)
	assert.Len(t, digestsOf(t, reg, testAccount, repo), 1)
}

func TestSweepRepo_Idempotent(t *testing.T) {
	reg := newTestRegistry()
	now := time.Now().UTC()
	repo := "team/app"
	seedTimedImage(t, reg, testAccount, repo, "sha256:olduntagged", now.AddDate(0, 0, -10))
	seedTimedImage(t, reg, testAccount, repo, "sha256:newuntagged", now.AddDate(0, 0, -2))
	putPolicy(t, reg, testAccount, repo, untaggedExpirePolicy)

	s := newSweeper(reg, testAccount)
	assert.Equal(t, 1, s.sweepRepo(context.Background(), testAccount, repo, now))
	assert.Equal(t, 0, s.sweepRepo(context.Background(), testAccount, repo, now)) // nothing left to expire
}

func TestSweepOnce_MultiAccount(t *testing.T) {
	reg := newTestRegistry()
	now := time.Now().UTC()
	const acct2 = "000000000001"
	policy := `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"any","countType":"imageCountMoreThan","countNumber":1},"action":{"type":"expire"}}]}`

	seedTimedImage(t, reg, testAccount, "team/app", "sha256:a1", now.AddDate(0, 0, -1), "n")
	seedTimedImage(t, reg, testAccount, "team/app", "sha256:a2", now.AddDate(0, 0, -2), "o")
	putPolicy(t, reg, testAccount, "team/app", policy)

	seedTimedImage(t, reg, acct2, "team/web", "sha256:b1", now.AddDate(0, 0, -1), "n")
	seedTimedImage(t, reg, acct2, "team/web", "sha256:b2", now.AddDate(0, 0, -2), "o")
	putPolicy(t, reg, acct2, "team/web", policy)

	deleted := newSweeper(reg, testAccount, acct2).sweepOnce(context.Background(), now)
	assert.Equal(t, 2, deleted) // one per account
	assert.Len(t, digestsOf(t, reg, testAccount, "team/app"), 1)
	assert.Len(t, digestsOf(t, reg, acct2, "team/web"), 1)
}

func TestSweepOnce_AccountsError(t *testing.T) {
	reg := newTestRegistry()
	s := NewLifecycleSweeper(reg, func() ([]string, error) {
		return nil, assert.AnError
	}, time.Hour)
	assert.Equal(t, 0, s.sweepOnce(context.Background(), time.Now().UTC()))
}

func TestNewLifecycleSweeper_DefaultInterval(t *testing.T) {
	reg := newTestRegistry()
	s := NewLifecycleSweeper(reg, func() ([]string, error) { return nil, nil }, 0)
	assert.Equal(t, DefaultLifecycleSweepInterval, s.interval)

	s = NewLifecycleSweeper(reg, func() ([]string, error) { return nil, nil }, -5)
	assert.Equal(t, DefaultLifecycleSweepInterval, s.interval)

	s = NewLifecycleSweeper(reg, func() ([]string, error) { return nil, nil }, time.Minute)
	assert.Equal(t, time.Minute, s.interval)
}

// TestSweeperRun drives the ticker loop: with a tiny interval and an expiring
// image, the first tick must delete it; cancelling the context stops Run.
func TestSweeperRun(t *testing.T) {
	reg := newTestRegistry()
	now := time.Now().UTC()
	repo := "team/app"
	seedTimedImage(t, reg, testAccount, repo, "sha256:olduntagged", now.AddDate(0, 0, -10))
	seedTimedImage(t, reg, testAccount, repo, "sha256:newuntagged", now.AddDate(0, 0, -2))
	putPolicy(t, reg, testAccount, repo, untaggedExpirePolicy)

	var swept atomic.Int64
	s := NewLifecycleSweeper(reg, func() ([]string, error) {
		swept.Add(1)
		return []string{testAccount}, nil
	}, time.Millisecond)
	s.now = func() time.Time { return now }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool {
		return len(digestsOf(t, reg, testAccount, repo)) == 1
	}, time.Second, 2*time.Millisecond, "expired image not swept")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancel")
	}
	assert.Positive(t, swept.Load())
}

// TestSweepRepo_InvalidStoredPolicy exercises the evaluate-error branch: a
// malformed stored policy is logged and skipped, deleting nothing.
func TestSweepRepo_InvalidStoredPolicy(t *testing.T) {
	reg := newTestRegistry()
	now := time.Now().UTC()
	repo := "team/app"
	seedTimedImage(t, reg, testAccount, repo, "sha256:x", now.AddDate(0, 0, -100))
	putPolicy(t, reg, testAccount, repo, "{not valid json")

	deleted := newSweeper(reg, testAccount).sweepRepo(context.Background(), testAccount, repo, now)
	assert.Equal(t, 0, deleted)
	assert.Len(t, digestsOf(t, reg, testAccount, repo), 1)
}

// TestSweepRepo_PolicyWithoutRepo exercises the ListImages-not-found branch: a
// policy stored for a repo with no manifests yields nothing to expire.
func TestSweepRepo_PolicyWithoutRepo(t *testing.T) {
	reg := newTestRegistry()
	now := time.Now().UTC()
	putPolicy(t, reg, testAccount, "ghost", untaggedExpirePolicy)

	deleted := newSweeper(reg, testAccount).sweepRepo(context.Background(), testAccount, "ghost", now)
	assert.Equal(t, 0, deleted)
}

func TestSameTagSet(t *testing.T) {
	assert.True(t, sameTagSet(nil, nil))
	assert.True(t, sameTagSet([]string{"a", "b"}, []string{"b", "a"}))
	assert.False(t, sameTagSet([]string{"a"}, []string{"a", "b"}))
	assert.False(t, sameTagSet([]string{"a"}, []string{"b"}))
	assert.False(t, sameTagSet(nil, []string{"a"}))
}
