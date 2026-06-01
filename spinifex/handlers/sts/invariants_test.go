package handlers_sts

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Bucket-prefix invariant for the session-credentials KV bucket. A session AKID
// landing in the long-lived bucket — or a long-lived AKID landing in the
// session bucket — would be resolved by the SigV4 path matching its on-wire
// prefix, bypassing that path's invariants. The writer-side guard in
// putSessionCredential is the load-bearing defence; this test exercises it
// directly so a regression that adds a new writer path bypassing the helper
// also fails CI.

func TestInvariant_SessionCredentialsBucket_RejectsNonASIAPrefix(t *testing.T) {
	bucket := setupBucket(t)

	cases := []struct {
		name string
		akid string
	}{
		{"AKIA prefix (long-lived)", "AKIA0123456789ABCDEF"},
		{"AROA prefix (role)", "AROA0123456789ABCDEF"},
		{"empty", ""},
		{"lowercase asia", "asia0123456789ABCDEF"},
		{"random", "FOOBAR0123456789ABCD"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cred := newTestSessionCredential(tc.akid)
			err := putSessionCredential(bucket, cred)
			require.Error(t, err, "putSessionCredential accepted invalid prefix %q", tc.akid)
			assert.Contains(t, err.Error(), SessionAccessKeyIDPrefix)
		})
	}
}

func TestInvariant_SessionCredentialsBucket_AcceptsASIAPrefix(t *testing.T) {
	bucket := setupBucket(t)

	akid := SessionAccessKeyIDPrefix + "0123456789ABCDEF"
	cred := newTestSessionCredential(akid)
	require.NoError(t, putSessionCredential(bucket, cred))
}

// Cross-cluster anti-replay invariant. A presigned sts:GetCallerIdentity URL
// signed under cluster-A's name MUST NOT verify when presented to cluster-B.
// The defence is the X-K8s-Aws-Id header participating in SigV4
// canonicalisation: the verifier reconstructs the canonical request with
// X-K8s-Aws-Id = expectedClusterName, so a mismatch produces a different
// signature.
//
// Three regressions this guards against:
//  1. x-k8s-aws-id silently dropped from SignedHeaders enforcement.
//  2. Verifier reconstructing the header value from the URL instead of from
//     expectedClusterName (would always self-match).
//  3. Verifier short-circuiting on equal AccessKeyId without recomputing
//     the SigV4 signature.
//
// On breach the EKS token webhook would accept a token forged by anyone with
// console access to one cluster against any other cluster sharing the same
// IAM trust — a complete authn bypass.
func TestInvariant_PresignedGetCallerIdentity_CrossClusterReplayRejected(t *testing.T) {
	svc, _ := newTestSetup(t)
	akid, secret := seedAccessKey(t, svc, testCallerAccountID, "invariant-xcluster")

	signedAt := time.Now().UTC().Truncate(time.Second)
	withFrozenTime(t, signedAt)

	urlA := presignTestURL(t, akid, secret, "cluster-A", signedAt, 900)

	// Sanity: cluster-A accepts its own URL (so a failure below is a real
	// cross-cluster rejection, not a generic verifier break).
	_, err := svc.VerifyPresignedGetCallerIdentity(urlA, "cluster-A")
	require.NoError(t, err, "self-verification must succeed; otherwise replay test is meaningless")

	// Invariant: any other cluster name rejects cluster-A's URL.
	// Note: SigV4 canonicalisation trims leading/trailing whitespace from
	// header values, so a trailing-space variant intentionally is not in this
	// list — it would self-match per AWS's wire spec.
	otherClusters := []string{"cluster-B", "cluster-A-prod", "CLUSTER-A", "cluster-A-1"}
	for _, other := range otherClusters {
		t.Run(other, func(t *testing.T) {
			_, err := svc.VerifyPresignedGetCallerIdentity(urlA, other)
			require.Error(t, err, "URL signed for cluster-A must not verify under %q", other)
			assert.Equal(t, awserrors.ErrorInvalidIdentityToken, err.Error(),
				"cross-cluster replay must surface as InvalidIdentityToken (401-equivalent)")
		})
	}
}
