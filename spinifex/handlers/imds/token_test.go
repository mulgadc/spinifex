package handlers_imds

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenStore_IssueAndValidate(t *testing.T) {
	s := newTokenStore()
	now := time.Unix(1_700_000_000, 0)

	token, err := s.issue("eni-aaa", 60*time.Second, now)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	assert.True(t, s.validate(token, "eni-aaa", now), "fresh token for its ENI must validate")
	assert.True(t, s.validate(token, "eni-aaa", now.Add(59*time.Second)), "token valid until TTL")
}

func TestTokenStore_WrongENIRejected(t *testing.T) {
	s := newTokenStore()
	now := time.Unix(1_700_000_000, 0)
	token, err := s.issue("eni-aaa", 60*time.Second, now)
	require.NoError(t, err)

	// A token issued to eni-aaa must not validate for eni-bbb — otherwise a
	// guest could replay a peer's token to read the peer's session.
	assert.False(t, s.validate(token, "eni-bbb", now))
}

func TestTokenStore_ExpiredRejectedAndEvicted(t *testing.T) {
	s := newTokenStore()
	now := time.Unix(1_700_000_000, 0)
	token, err := s.issue("eni-aaa", 1*time.Second, now)
	require.NoError(t, err)

	assert.False(t, s.validate(token, "eni-aaa", now.Add(2*time.Second)), "expired token must be rejected")
	// Lazy eviction: the failed validate above dropped it, so even at a valid
	// time it no longer exists.
	assert.False(t, s.validate(token, "eni-aaa", now))
}

func TestTokenStore_UnknownAndEmptyRejected(t *testing.T) {
	s := newTokenStore()
	now := time.Unix(1_700_000_000, 0)
	assert.False(t, s.validate("", "eni-aaa", now))
	assert.False(t, s.validate("never-issued", "eni-aaa", now))
}

func TestTokenStore_Sweep(t *testing.T) {
	s := newTokenStore()
	now := time.Unix(1_700_000_000, 0)
	live, _ := s.issue("eni-live", 3600*time.Second, now)
	dead, _ := s.issue("eni-dead", 1*time.Second, now)

	s.sweep(now.Add(2 * time.Second))

	assert.True(t, s.validate(live, "eni-live", now.Add(2*time.Second)), "live token survives sweep")
	assert.False(t, s.validate(dead, "eni-dead", now), "expired token swept")
}

func TestTokenStore_IssueUnique(t *testing.T) {
	s := newTokenStore()
	now := time.Unix(1_700_000_000, 0)
	a, _ := s.issue("eni-aaa", 60*time.Second, now)
	b, _ := s.issue("eni-aaa", 60*time.Second, now)
	assert.NotEqual(t, a, b, "each issued token must be distinct")
}

func TestParseTokenTTL(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		secs int
	}{
		{"", false, 0},
		{"abc", false, 0},
		{"0", false, 0},
		{"21601", false, 0},
		{"-5", false, 0},
		{"1", true, 1},
		{"21600", true, 21600},
		{"60", true, 60},
	}
	for _, c := range cases {
		d, ok := parseTokenTTL(c.in)
		assert.Equal(t, c.ok, ok, "ttl=%q", c.in)
		if ok {
			assert.Equal(t, c.secs, int(d.Seconds()), "ttl=%q", c.in)
		}
	}
}
