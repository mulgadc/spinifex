package ecr

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestKVMetaStore_FullCycle exercises the JetStream-backed MetaStore against an
// embedded JetStream server, covering the production CAS path.
func TestKVMetaStore_FullCycle(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewKVMetaStore(js)
	const acct = "000000000000"

	// Repo create + list (also lazily creates the bucket).
	require.NoError(t, store.PutRepo(acct, RepoMeta{Name: "team/app", CreatedAt: time.Now()}))
	require.NoError(t, store.PutRepo(acct, RepoMeta{Name: "team/web", CreatedAt: time.Now()}))
	got, err := store.GetRepo(acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, "team/app", got.Name)
	_, err = store.GetRepo(acct, "team/ghost")
	assert.ErrorIs(t, err, ErrNotFound)

	repos, err := store.ListRepos(acct)
	require.NoError(t, err)
	assert.Equal(t, []string{"team/app", "team/web"}, repos)

	// Tags.
	require.NoError(t, store.PutTag(acct, "team/app", "v1", "sha256:aaa"))
	require.NoError(t, store.PutTag(acct, "team/app", "v2", "sha256:bbb"))
	d, err := store.GetTag(acct, "team/app", "v1")
	require.NoError(t, err)
	assert.Equal(t, "sha256:aaa", d)
	tags, err := store.ListTags(acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, []string{"v1", "v2"}, tags)
	require.NoError(t, store.DeleteTag(acct, "team/app", "v1"))
	_, err = store.GetTag(acct, "team/app", "v1")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, store.DeleteTag(acct, "team/app", "missing"), ErrNotFound)

	// Manifest meta.
	require.NoError(t, store.PutManifestMeta(acct, "team/app", ManifestMeta{
		Digest: "sha256:ccc", MediaType: "application/json", Size: 12,
	}))
	mm, err := store.GetManifestMeta(acct, "team/app", "sha256:ccc")
	require.NoError(t, err)
	assert.Equal(t, int64(12), mm.Size)
	_, err = store.GetManifestMeta(acct, "team/app", "sha256:zzz")
	assert.ErrorIs(t, err, ErrNotFound)

	// Upload CAS lifecycle.
	rev, err := store.PutUpload(acct, "u1", UploadState{RepoName: "team/app"})
	require.NoError(t, err)
	st, gotRev, err := store.GetUpload(acct, "u1")
	require.NoError(t, err)
	assert.Equal(t, rev, gotRev)

	_, err = store.UpdateUpload(acct, "u1", st, gotRev+99)
	assert.ErrorIs(t, err, ErrConflict)

	st.CommittedBytes = 5
	newRev, err := store.UpdateUpload(acct, "u1", st, gotRev)
	require.NoError(t, err)
	assert.Greater(t, newRev, gotRev)

	require.NoError(t, store.DeleteUpload(acct, "u1"))
	_, _, err = store.GetUpload(acct, "u1")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, store.DeleteUpload(acct, "u1"), ErrNotFound)

	// Empty-bucket listing returns no repos for a fresh account.
	empty, err := store.ListRepos("999999999999")
	require.NoError(t, err)
	assert.Empty(t, empty)
}
