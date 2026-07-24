package ecr

import (
	"context"

	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestKVMetaStore_FullCycle exercises the JetStream-backed MetaStore against an
// embedded JetStream server, covering the production CAS path.
func TestKVMetaStore_FullCycle(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	store := NewKVMetaStore(js)
	const acct = "000000000000"

	// Repo create + list (also lazily creates the bucket).
	require.NoError(t, store.PutRepo(context.Background(), acct, RepoMeta{Name: "team/app", CreatedAt: time.Now()}))
	require.NoError(t, store.PutRepo(context.Background(), acct, RepoMeta{Name: "team/web", CreatedAt: time.Now()}))
	got, err := store.GetRepo(context.Background(), acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, "team/app", got.Name)
	_, err = store.GetRepo(context.Background(), acct, "team/ghost")
	assert.ErrorIs(t, err, ErrNotFound)

	repos, err := store.ListRepos(context.Background(), acct)
	require.NoError(t, err)
	assert.Equal(t, []string{"team/app", "team/web"}, repos)

	// Tags.
	require.NoError(t, store.PutTag(context.Background(), acct, "team/app", "v1", "sha256:aaa"))
	require.NoError(t, store.PutTag(context.Background(), acct, "team/app", "v2", "sha256:bbb"))
	d, err := store.GetTag(context.Background(), acct, "team/app", "v1")
	require.NoError(t, err)
	assert.Equal(t, "sha256:aaa", d)
	tags, err := store.ListTags(context.Background(), acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, []string{"v1", "v2"}, tags)
	require.NoError(t, store.DeleteTag(context.Background(), acct, "team/app", "v1"))
	_, err = store.GetTag(context.Background(), acct, "team/app", "v1")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, store.DeleteTag(context.Background(), acct, "team/app", "missing"), ErrNotFound)

	// Manifest meta.
	require.NoError(t, store.PutManifestMeta(context.Background(), acct, "team/app", ManifestMeta{
		Digest: "sha256:ccc", MediaType: "application/json", Size: 12,
	}))
	mm, err := store.GetManifestMeta(context.Background(), acct, "team/app", "sha256:ccc")
	require.NoError(t, err)
	assert.Equal(t, int64(12), mm.Size)
	_, err = store.GetManifestMeta(context.Background(), acct, "team/app", "sha256:zzz")
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, store.DeleteManifestMeta(context.Background(), acct, "team/app", "sha256:ccc"))
	_, err = store.GetManifestMeta(context.Background(), acct, "team/app", "sha256:ccc")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, store.DeleteManifestMeta(context.Background(), acct, "team/app", "sha256:zzz"), ErrNotFound)

	// Upload CAS lifecycle.
	rev, err := store.PutUpload(context.Background(), acct, "u1", UploadState{RepoName: "team/app"})
	require.NoError(t, err)
	st, gotRev, err := store.GetUpload(context.Background(), acct, "u1")
	require.NoError(t, err)
	assert.Equal(t, rev, gotRev)

	_, err = store.UpdateUpload(context.Background(), acct, "u1", st, gotRev+99)
	assert.ErrorIs(t, err, ErrConflict)

	st.CommittedBytes = 5
	newRev, err := store.UpdateUpload(context.Background(), acct, "u1", st, gotRev)
	require.NoError(t, err)
	assert.Greater(t, newRev, gotRev)

	require.NoError(t, store.DeleteUpload(context.Background(), acct, "u1"))
	_, _, err = store.GetUpload(context.Background(), acct, "u1")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, store.DeleteUpload(context.Background(), acct, "u1"), ErrNotFound)

	// Empty-bucket listing returns no repos for a fresh account.
	empty, err := store.ListRepos(context.Background(), "999999999999")
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestKVMetaStore_DeleteRepo proves the cascade removes a repo's meta, policy,
// tags, and manifests, leaves a same-prefix nested repo intact, and reports
// ErrNotFound for an absent repo.
func TestKVMetaStore_DeleteRepo(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	store := NewKVMetaStore(js)
	const acct = "000000000000"
	const dig = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	require.NoError(t, store.PutRepo(context.Background(), acct, RepoMeta{Name: "team/app", CreatedAt: time.Now()}))
	require.NoError(t, store.PutRepo(context.Background(), acct, RepoMeta{Name: "team/app/sub", CreatedAt: time.Now()}))
	require.NoError(t, store.PutTag(context.Background(), acct, "team/app", "v1", dig))
	require.NoError(t, store.PutManifestMeta(context.Background(), acct, "team/app", ManifestMeta{Digest: dig, MediaType: "x", Size: 1}))
	require.NoError(t, store.PutRepoPolicy(context.Background(), acct, "team/app", []byte(`{}`)))
	require.NoError(t, store.PutLifecyclePolicy(context.Background(), acct, "team/app", []byte(`{}`)))

	manifests, err := store.ListManifests(context.Background(), acct, "team/app")
	require.NoError(t, err)
	assert.Len(t, manifests, 1)

	require.NoError(t, store.DeleteRepo(context.Background(), acct, "team/app"))

	_, err = store.GetRepo(context.Background(), acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)
	tags, err := store.ListTags(context.Background(), acct, "team/app")
	require.NoError(t, err)
	assert.Empty(t, tags)
	manifests, err = store.ListManifests(context.Background(), acct, "team/app")
	require.NoError(t, err)
	assert.Empty(t, manifests)
	_, err = store.GetRepoPolicy(context.Background(), acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = store.GetLifecyclePolicy(context.Background(), acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)

	// The same-prefix nested repo must survive.
	got, err := store.GetRepo(context.Background(), acct, "team/app/sub")
	require.NoError(t, err)
	assert.Equal(t, "team/app/sub", got.Name)

	assert.ErrorIs(t, store.DeleteRepo(context.Background(), acct, "team/ghost"), ErrNotFound)
}

// TestKVMetaStore_RepoPolicy exercises the policy passthrough against the
// JetStream-backed store and proves the policy key shares the repo bucket
// without being mistaken for a repository by ListRepos.
func TestKVMetaStore_RepoPolicy(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	store := NewKVMetaStore(js)
	const acct = "000000000000"
	const policy = `{"Version":"2012-10-17","Statement":[]}`

	require.NoError(t, store.PutRepo(context.Background(), acct, RepoMeta{Name: "team/app", CreatedAt: time.Now()}))

	_, err := store.GetRepoPolicy(context.Background(), acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, store.PutRepoPolicy(context.Background(), acct, "team/app", []byte(policy)))
	got, err := store.GetRepoPolicy(context.Background(), acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, policy, string(got))

	// The policy key must not appear as a repository.
	repos, err := store.ListRepos(context.Background(), acct)
	require.NoError(t, err)
	assert.Equal(t, []string{"team/app"}, repos)

	deleted, err := store.DeleteRepoPolicy(context.Background(), acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, policy, string(deleted))
	_, err = store.DeleteRepoPolicy(context.Background(), acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestKVMetaStore_LifecyclePolicy exercises the lifecycle-policy passthrough
// against the JetStream-backed store and proves the lifecycle key shares the
// repo bucket without being mistaken for a repository by ListRepos.
func TestKVMetaStore_LifecyclePolicy(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	store := NewKVMetaStore(js)
	const acct = "000000000000"
	const policy = `{"rules":[{"rulePriority":1,"selection":{"tagStatus":"untagged","countType":"sinceImagePushed","countUnit":"days","countNumber":14},"action":{"type":"expire"}}]}`

	require.NoError(t, store.PutRepo(context.Background(), acct, RepoMeta{Name: "team/app", CreatedAt: time.Now()}))

	_, err := store.GetLifecyclePolicy(context.Background(), acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, store.PutLifecyclePolicy(context.Background(), acct, "team/app", []byte(policy)))
	got, err := store.GetLifecyclePolicy(context.Background(), acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, policy, string(got))

	// The lifecycle key must not appear as a repository.
	repos, err := store.ListRepos(context.Background(), acct)
	require.NoError(t, err)
	assert.Equal(t, []string{"team/app"}, repos)

	deleted, err := store.DeleteLifecyclePolicy(context.Background(), acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, policy, string(deleted))
	_, err = store.DeleteLifecyclePolicy(context.Background(), acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)
}
