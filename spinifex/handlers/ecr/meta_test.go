package ecr

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateRepoName(t *testing.T) {
	valid := []string{"ab", "team/app", "a/b/c", "my-repo.name_1", "x0/y1/z2"}
	for _, n := range valid {
		assert.NoError(t, ValidateRepoName(n), "expected %q valid", n)
	}
	invalid := []string{"A", "Team/App", "a", "/leading", "trailing/", "a//b", strings.Repeat("a", 257)}
	for _, n := range invalid {
		assert.Error(t, ValidateRepoName(n), "expected %q invalid", n)
	}
}

func TestValidateDigest(t *testing.T) {
	good := "sha256:" + strings.Repeat("a", 64)
	assert.True(t, ValidateDigest(good))
	assert.False(t, ValidateDigest("sha256:abc"))
	assert.False(t, ValidateDigest("md5:"+strings.Repeat("a", 64)))
	assert.False(t, ValidateDigest("sha256:"+strings.Repeat("Z", 64)))
}

func TestKVKeyHelpers(t *testing.T) {
	assert.Equal(t, "ecr-account-000000000000", KVAccountBucket("000000000000"))
	assert.Equal(t, "repos/team/app/meta", KVRepoMetaKey("team/app"))
	assert.Equal(t, "repos/team/app/tags/", KVTagsPrefix("team/app"))
	assert.Equal(t, "repos/team/app/tags/v1", KVTagKey("team/app", "v1"))
	assert.Equal(t, "uploads/u1", KVUploadKey("u1"))
	assert.True(t, strings.HasPrefix(KVManifestKey("r", "sha256:abc"), "repos/r/manifests/sha256-abc"))
	assert.Equal(t, "sha256-abc", DigestToken("sha256:abc"))
}

func TestMemoryMetaStore_RepoRoundtrip(t *testing.T) {
	m := NewMemoryMetaStore()
	const acct = "000000000000"

	_, err := m.GetRepo(acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, m.PutRepo(acct, RepoMeta{Name: "team/app", CreatedAt: time.Now()}))
	require.NoError(t, m.PutRepo(acct, RepoMeta{Name: "team/web", CreatedAt: time.Now()}))

	got, err := m.GetRepo(acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, "team/app", got.Name)

	repos, err := m.ListRepos(acct)
	require.NoError(t, err)
	assert.Equal(t, []string{"team/app", "team/web"}, repos)
}

func TestMemoryMetaStore_TagsAndManifests(t *testing.T) {
	m := NewMemoryMetaStore()
	const acct = "000000000000"

	require.NoError(t, m.PutTag(acct, "r", "v1", "sha256:aaa"))
	require.NoError(t, m.PutTag(acct, "r", "v2", "sha256:bbb"))
	d, err := m.GetTag(acct, "r", "v1")
	require.NoError(t, err)
	assert.Equal(t, "sha256:aaa", d)

	tags, err := m.ListTags(acct, "r")
	require.NoError(t, err)
	assert.Equal(t, []string{"v1", "v2"}, tags)

	require.NoError(t, m.DeleteTag(acct, "r", "v1"))
	_, err = m.GetTag(acct, "r", "v1")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, m.DeleteTag(acct, "r", "missing"), ErrNotFound)

	require.NoError(t, m.PutManifestMeta(acct, "r", ManifestMeta{Digest: "sha256:ccc", MediaType: "x", Size: 7}))
	mm, err := m.GetManifestMeta(acct, "r", "sha256:ccc")
	require.NoError(t, err)
	assert.Equal(t, int64(7), mm.Size)
	_, err = m.GetManifestMeta(acct, "r", "sha256:zzz")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestMemoryMetaStore_UploadCAS(t *testing.T) {
	m := NewMemoryMetaStore()
	const acct = "000000000000"

	rev, err := m.PutUpload(acct, "u1", UploadState{RepoName: "r"})
	require.NoError(t, err)

	st, gotRev, err := m.GetUpload(acct, "u1")
	require.NoError(t, err)
	assert.Equal(t, rev, gotRev)
	assert.Equal(t, "r", st.RepoName)

	// Stale revision is rejected.
	_, err = m.UpdateUpload(acct, "u1", st, rev+99)
	assert.ErrorIs(t, err, ErrConflict)

	st.CommittedBytes = 10
	newRev, err := m.UpdateUpload(acct, "u1", st, gotRev)
	require.NoError(t, err)
	assert.Greater(t, newRev, gotRev)

	require.NoError(t, m.DeleteUpload(acct, "u1"))
	_, _, err = m.GetUpload(acct, "u1")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, m.DeleteUpload(acct, "u1"), ErrNotFound)
}

func TestTrimSuffixMeta(t *testing.T) {
	assert.Equal(t, "team/app", trimSuffixMeta("repos/team/app/meta"))
}

func TestMemoryMetaStore_DeleteRepoAndListManifests(t *testing.T) {
	m := NewMemoryMetaStore()
	const acct = "000000000000"

	require.NoError(t, m.PutRepo(acct, RepoMeta{Name: "team/app", CreatedAt: time.Now()}))
	require.NoError(t, m.PutRepo(acct, RepoMeta{Name: "team/web", CreatedAt: time.Now()}))
	require.NoError(t, m.PutTag(acct, "team/app", "v1", "sha256:aaa"))
	require.NoError(t, m.PutManifestMeta(acct, "team/app", ManifestMeta{Digest: "sha256:ccc", MediaType: "x", Size: 3}))
	require.NoError(t, m.PutRepoPolicy(acct, "team/app", []byte(`{}`)))

	digs, err := m.ListManifests(acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, []string{"sha256:ccc"}, digs)

	require.NoError(t, m.DeleteRepo(acct, "team/app"))
	_, err = m.GetRepo(acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)
	tags, err := m.ListTags(acct, "team/app")
	require.NoError(t, err)
	assert.Empty(t, tags)
	digs, err = m.ListManifests(acct, "team/app")
	require.NoError(t, err)
	assert.Empty(t, digs)
	_, err = m.GetRepoPolicy(acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)

	// Other repo untouched.
	got, err := m.GetRepo(acct, "team/web")
	require.NoError(t, err)
	assert.Equal(t, "team/web", got.Name)

	assert.ErrorIs(t, m.DeleteRepo(acct, "team/ghost"), ErrNotFound)
}

func TestMemoryMetaStore_RepoPolicy(t *testing.T) {
	m := NewMemoryMetaStore()
	const acct = "000000000000"
	const policy = `{"Version":"2012-10-17"}`

	_, err := m.GetRepoPolicy(acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = m.DeleteRepoPolicy(acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, m.PutRepoPolicy(acct, "team/app", []byte(policy)))
	got, err := m.GetRepoPolicy(acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, policy, string(got))

	deleted, err := m.DeleteRepoPolicy(acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, policy, string(deleted))
	_, err = m.GetRepoPolicy(acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)
}
