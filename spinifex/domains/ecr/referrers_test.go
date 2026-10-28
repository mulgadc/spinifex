package ecr

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	refSubject = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	refSigA    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	refSigB    = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func referrerMeta(digest, subject string) ManifestMeta {
	return ManifestMeta{
		Digest:        digest,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Size:          42,
		SubjectDigest: subject,
		ArtifactType:  "application/vnd.example.sig.v1",
		Annotations:   map[string]string{"org.example.key": "v"},
	}
}

// referrerStores runs a test against every MetaStore implementation that keeps
// its own referrers state.
func referrerStores(t *testing.T) map[string]MetaStore {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return map[string]MetaStore{
		"memory": NewMemoryMetaStore(),
		"kv":     NewKVMetaStore(testutil.NewJetStream(t, nc)),
	}
}

func TestListReferrers(t *testing.T) {
	ctx := context.Background()
	for name, store := range referrerStores(t) {
		t.Run(name, func(t *testing.T) {
			acct, repo := "111111111111", "team/app"

			got, err := store.ListReferrers(ctx, acct, repo, refSubject)
			require.NoError(t, err)
			assert.Empty(t, got, "a subject with no referrers lists empty")

			require.NoError(t, store.PutManifestMeta(ctx, acct, repo, referrerMeta(refSigB, refSubject)))
			require.NoError(t, store.PutManifestMeta(ctx, acct, repo, referrerMeta(refSigA, refSubject)))
			// Re-putting the same digest must not duplicate the entry.
			require.NoError(t, store.PutManifestMeta(ctx, acct, repo, referrerMeta(refSigA, refSubject)))

			got, err = store.ListReferrers(ctx, acct, repo, refSubject)
			require.NoError(t, err)
			require.Len(t, got, 2)
			assert.Equal(t, refSigA, got[0].Digest, "entries are ordered by digest")
			assert.Equal(t, refSigB, got[1].Digest)
			assert.Equal(t, ReferrerDescriptor{
				MediaType:    "application/vnd.oci.image.manifest.v1+json",
				Digest:       refSigA,
				Size:         42,
				ArtifactType: "application/vnd.example.sig.v1",
				Annotations:  map[string]string{"org.example.key": "v"},
			}, got[0])

			otherRepo, err := store.ListReferrers(ctx, acct, "team/other", refSubject)
			require.NoError(t, err)
			assert.Empty(t, otherRepo, "referrers do not cross repositories")
			otherAcct, err := store.ListReferrers(ctx, "222222222222", repo, refSubject)
			require.NoError(t, err)
			assert.Empty(t, otherAcct, "referrers do not cross accounts")

			require.NoError(t, store.DeleteManifestMeta(ctx, acct, repo, refSigA))
			got, err = store.ListReferrers(ctx, acct, repo, refSubject)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, refSigB, got[0].Digest)
		})
	}
}

func TestListReferrers_ManifestWithoutSubjectIsNotListed(t *testing.T) {
	ctx := context.Background()
	for name, store := range referrerStores(t) {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, store.PutManifestMeta(ctx, "111111111111", "r", referrerMeta(refSigA, "")))
			got, err := store.ListReferrers(ctx, "111111111111", "r", refSubject)
			require.NoError(t, err)
			assert.Empty(t, got)
		})
	}
}
