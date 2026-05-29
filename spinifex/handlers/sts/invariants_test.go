package handlers_sts

import (
	"testing"

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
