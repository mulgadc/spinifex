package issuer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// RenewBefore is how long before expiry a session or certificate is renewed.
const RenewBefore = 5 * time.Minute

// ErrConnectionNotActive means the authoritative record no longer allows
// AWS access for the connection.
var ErrConnectionNotActive = errors.New("connection is not active")

// ErrRoleNotAllowed means the role is not one of the connection's roles.
var ErrRoleNotAllowed = errors.New("role is not bound to the connection")

// StateChecker reads the authoritative connection state. It returns
// ErrConnectionNotActive (possibly wrapped) when use must stop.
type StateChecker interface {
	CheckActive(ctx context.Context, accountID, connectionID string) error
}

// Connection names the AWS side of one connection. Roles are the role ARNs
// that the connection record holds; no other role can be assumed.
type Connection struct {
	AccountID      string
	ConnectionID   string
	TrustAnchorARN string
	ProfileARN     string
	Roles          []string
}

// Cache keeps migration certificates and AWS sessions in process memory,
// one session per account, connection and role. Nothing is written to disk.
type Cache struct {
	issuer  CertIssuer
	creator SessionCreator
	state   StateChecker
	now     func() time.Time

	mu    sync.Mutex
	conns map[connKey]*connEntry
}

type connKey struct{ account, connection string }

type connEntry struct {
	mu       sync.Mutex
	cert     *ClientCertificate
	sessions map[string]Session
}

func (e *connEntry) clear() {
	e.cert = nil
	e.sessions = map[string]Session{}
}

// NewCache builds a Cache. now is the clock.
func NewCache(issuer CertIssuer, creator SessionCreator, state StateChecker, now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{issuer: issuer, creator: creator, state: state, now: now, conns: map[connKey]*connEntry{}}
}

// Credentials returns a session for roleARN. It reads the connection state
// after it holds the entry, so a caller that waited behind a refresh still
// sees a disconnect, and again after a refresh before it returns. When the
// connection is not active it drops everything cached for it.
func (c *Cache) Credentials(ctx context.Context, conn Connection, roleARN string) (Session, error) {
	if !slices.Contains(conn.Roles, roleARN) {
		return Session{}, ErrRoleNotAllowed
	}
	key := connKey{conn.AccountID, conn.ConnectionID}
	e := c.entry(key)
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := c.checkActive(ctx, key, e); err != nil {
		return Session{}, err
	}
	if s, ok := e.sessions[roleARN]; ok && s.Expiration.Sub(c.now()) > RenewBefore {
		return s, nil
	}
	if e.cert == nil || e.cert.Certificate.NotAfter.Sub(c.now()) <= RenewBefore {
		cert, err := c.issuer.Issue(conn.AccountID, conn.ConnectionID)
		if err != nil {
			return Session{}, err
		}
		e.cert = cert
	}
	s, err := c.creator.CreateSession(ctx, e.cert, SessionRequest{
		TrustAnchorARN: conn.TrustAnchorARN, ProfileARN: conn.ProfileARN, RoleARN: roleARN,
	})
	if err != nil {
		delete(e.sessions, roleARN)
		return Session{}, err
	}
	if err := c.checkActive(ctx, key, e); err != nil {
		return Session{}, err
	}
	e.sessions[roleARN] = s
	return s, nil
}

// Drop forgets the certificate and every session of a connection.
func (c *Cache) Drop(accountID, connectionID string) {
	key := connKey{accountID, connectionID}
	c.mu.Lock()
	e := c.conns[key]
	delete(c.conns, key)
	c.mu.Unlock()
	if e != nil {
		e.mu.Lock()
		e.clear()
		e.mu.Unlock()
	}
}

// DropAccount forgets the certificates and sessions of every connection of
// accountID.
func (c *Cache) DropAccount(accountID string) {
	c.mu.Lock()
	var dropped []*connEntry
	for key, e := range c.conns {
		if key.account == accountID {
			dropped = append(dropped, e)
			delete(c.conns, key)
		}
	}
	c.mu.Unlock()
	for _, e := range dropped {
		e.mu.Lock()
		e.clear()
		e.mu.Unlock()
	}
}

// Accounts lists the accounts that have a cache entry, each once.
func (c *Cache) Accounts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for key := range c.conns {
		if !seen[key.account] {
			seen[key.account] = true
			out = append(out, key.account)
		}
	}
	slices.Sort(out)
	return out
}

// Sweep drops sessions and certificates that have expired, so an idle
// connection holds no credential past its lifetime.
func (c *Cache) Sweep() {
	c.mu.Lock()
	entries := make([]*connEntry, 0, len(c.conns))
	for _, e := range c.conns {
		entries = append(entries, e)
	}
	c.mu.Unlock()

	now := c.now()
	for _, e := range entries {
		e.mu.Lock()
		for role, s := range e.sessions {
			if !s.Expiration.After(now) {
				delete(e.sessions, role)
			}
		}
		if e.cert != nil && !e.cert.Certificate.NotAfter.After(now) {
			e.cert = nil
		}
		e.mu.Unlock()
	}
}

// checkActive reads the connection state with e locked by the caller. When
// the connection is not active it clears e and removes it from the cache.
func (c *Cache) checkActive(ctx context.Context, key connKey, e *connEntry) error {
	err := c.state.CheckActive(ctx, key.account, key.connection)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrConnectionNotActive) {
		e.clear()
		c.mu.Lock()
		if c.conns[key] == e {
			delete(c.conns, key)
		}
		c.mu.Unlock()
	}
	return fmt.Errorf("connection state check: %w", err)
}

func (c *Cache) entry(key connKey) *connEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.conns[key]
	if e == nil {
		e = &connEntry{sessions: map[string]Session{}}
		c.conns[key] = e
	}
	return e
}
