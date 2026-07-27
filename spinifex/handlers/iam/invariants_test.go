package handlers_iam

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Bucket-prefix invariants. Long-lived (AKIA) and STS session (ASIA) AKIDs
// live in disjoint KV buckets. Each bucket MUST reject writes whose key does
// not start with the expected prefix; without this guard a misfiled record
// is resolved by the SigV4 path matching its on-wire prefix, bypassing the
// path's invariants (the AKIA path skips both expiry and X-Amz-Security-Token
// checks). The writer-side guards are the load-bearing defence — the SigV4
// prefix-first dispatch is the second line.
//
// These tests bypass the public IAMServiceImpl API and exercise the
// putAccessKey / createAccessKey helpers directly so a regression that adds
// a new writer path bypassing the helpers also fails CI.

func TestInvariant_AccessKeysBucket_RejectsNonAKIAPrefix(t *testing.T) {
	svc := setupTestIAMService(t)

	cases := []struct {
		name string
		akid string
	}{
		{"ASIA prefix (session)", "ASIA0123456789ABCDEF"},
		{"AROA prefix (role)", "AROA0123456789ABCDEF"},
		{"empty", ""},
		{"lowercase akia", "akia0123456789ABCDEF"},
		{"random", "FOOBAR0123456789ABCD"},
	}

	payload := []byte(`{}`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.putAccessKey(t.Context(), tc.akid, payload)
			require.Error(t, err, "putAccessKey accepted invalid prefix %q", tc.akid)
			assert.Contains(t, err.Error(), LongLivedAccessKeyIDPrefix)

			err = svc.createAccessKey(t.Context(), tc.akid, payload)
			require.Error(t, err, "createAccessKey accepted invalid prefix %q", tc.akid)
			assert.Contains(t, err.Error(), LongLivedAccessKeyIDPrefix)
		})
	}
}

func TestInvariant_AccessKeysBucket_AcceptsAKIAPrefix(t *testing.T) {
	svc := setupTestIAMService(t)

	akid := LongLivedAccessKeyIDPrefix + "0123456789ABCDEF"
	require.NoError(t, svc.putAccessKey(t.Context(), akid, []byte(`{}`)))

	entry, err := svc.accessKeysBucket.Get(t.Context(), akid)
	require.NoError(t, err)
	assert.Equal(t, []byte(`{}`), entry.Value())
}
