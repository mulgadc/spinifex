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

// tokenEntry binds an IMDSv2 token to its issuing ENI and expiry.
// Bound to the ENI (not the IP) so it survives DHCP-renew but dies on ENI detach.
type tokenEntry struct {
	eniID     string
	expiresAt time.Time
}

// tokenStore holds outstanding IMDSv2 tokens in memory. Tokens are a CSRF defence,
// not persisted; a vpcd restart drops them and SDKs reissue transparently.
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

// validate reports whether token is valid and was issued to expectedENI.
// Wrong-ENI tokens are rejected identically to unknown ones to prevent probing.
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

// sweep deletes expired tokens to prevent abandoned tokens from leaking.
func (s *tokenStore) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, entry := range s.tokens {
		if now.After(entry.expiresAt) {
			delete(s.tokens, token)
		}
	}
}
