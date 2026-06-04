package handlers_imds

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

const (
	// tokenTTLMin / tokenTTLMax bound the X-aws-ec2-metadata-token-ttl-seconds
	// header, matching AWS (1 second to 6 hours).
	tokenTTLMin = 1
	tokenTTLMax = 21600

	// tokenBytes is the random token length before base64url encoding.
	tokenBytes = 32
)

// tokenEntry binds an issued IMDSv2 token to the ENI that requested it (not the
// IP — so the session survives a DHCP-renew IP change but dies on ENI detach)
// and its expiry.
type tokenEntry struct {
	eniID     string
	expiresAt time.Time
}

// tokenStore holds outstanding IMDSv2 tokens in memory, per process. Tokens are
// a CSRF defence, not a security artefact, so they are never persisted; a
// vpcd restart drops them and SDKs reissue transparently.
type tokenStore struct {
	mu     sync.Mutex
	tokens map[string]tokenEntry
}

func newTokenStore() *tokenStore {
	return &tokenStore{tokens: make(map[string]tokenEntry)}
}

// issue mints a fresh token bound to eniID, valid for ttl from now.
func (s *tokenStore) issue(eniID string, ttl time.Duration, now time.Time) (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	s.tokens[token] = tokenEntry{eniID: eniID, expiresAt: now.Add(ttl)}
	s.mu.Unlock()
	return token, nil
}

// validate reports whether token is currently valid AND was issued to
// expectedENI. A wrong-ENI token is rejected exactly like an unknown one so the
// caller cannot probe which tokens exist. Expired tokens are evicted lazily.
func (s *tokenStore) validate(token, expectedENI string, now time.Time) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.tokens[token]
	if !ok {
		return false
	}
	if now.After(entry.expiresAt) {
		delete(s.tokens, token)
		return false
	}
	return entry.eniID == expectedENI
}

// sweep deletes every token that expired at or before now. Called on a 1-minute
// ticker so abandoned tokens (an SDK that issues then never reads) don't leak.
func (s *tokenStore) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, entry := range s.tokens {
		if now.After(entry.expiresAt) {
			delete(s.tokens, token)
		}
	}
}
