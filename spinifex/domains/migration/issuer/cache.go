package issuer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// RenewBefore is how long before expiry a session or certificate is renewed.
const RenewBefore = 5 * time.Minute

// ErrConnectionNotActive means the authoritative record no longer allows
// AWS access for the connection.
var ErrConnectionNotActive = errors.New("connection is not active")

// StateChecker reads the authoritative connection state. It returns
// ErrConnectionNotActive (possibly wrapped) when use must stop.
type StateChecker interface {
	CheckActive(ctx context.Context, connectionID string) error
}

// Connection names the AWS side of one connection.
type Connection struct {
	AccountID      string
	ConnectionID   string
	TrustAnchorARN string
	ProfileARN     string
}

// Cache keeps migration certificates and AWS sessions in process memory,
// one session per connection and role. Nothing is written to disk.
type Cache struct {
	issuer  CertIssuer
	creator SessionCreator
	state   StateChecker
	now     func() time.Time

	mu    sync.Mutex
	conns map[string]*connEntry
}

type connEntry struct {
	mu       sync.Mutex
	cert     *ClientCertificate
	sessions map[string]Session
}

// NewCache builds a Cache. now is the clock.
func NewCache(issuer CertIssuer, creator SessionCreator, state StateChecker, now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{issuer: issuer, creator: creator, state: state, now: now, conns: map[string]*connEntry{}}
}

// Credentials returns a session for roleARN. It checks the connection state
// before every use, and again before it asks AWS for a new session. When the
// connection is not active it drops everything cached for it.
func (c *Cache) Credentials(ctx context.Context, conn Connection, roleARN string) (Session, error) {
	if err := c.checkActive(ctx, conn.ConnectionID, false); err != nil {
		return Session{}, err
	}
	e := c.entry(conn.ConnectionID)
	e.mu.Lock()
	defer e.mu.Unlock()

	if s, ok := e.sessions[roleARN]; ok && s.Expiration.Sub(c.now()) > RenewBefore {
		return s, nil
	}
	if err := c.checkActive(ctx, conn.ConnectionID, true); err != nil {
		if errors.Is(err, ErrConnectionNotActive) {
			e.cert, e.sessions = nil, map[string]Session{}
		}
		return Session{}, err
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
	e.sessions[roleARN] = s
	return s, nil
}

// Drop forgets the certificate and every session of a connection.
func (c *Cache) Drop(connectionID string) {
	c.mu.Lock()
	e := c.conns[connectionID]
	delete(c.conns, connectionID)
	c.mu.Unlock()
	if e != nil {
		e.mu.Lock()
		e.cert = nil
		e.sessions = map[string]Session{}
		e.mu.Unlock()
	}
}

// checkActive reads the connection state. When the connection is not active
// it drops the cache entry, unless the caller holds the entry lock, in which
// case the caller clears the entry itself.
func (c *Cache) checkActive(ctx context.Context, connectionID string, entryLocked bool) error {
	err := c.state.CheckActive(ctx, connectionID)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrConnectionNotActive) && !entryLocked {
		c.Drop(connectionID)
	}
	return fmt.Errorf("connection state check: %w", err)
}

func (c *Cache) entry(connectionID string) *connEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.conns[connectionID]
	if e == nil {
		e = &connEntry{sessions: map[string]Session{}}
		c.conns[connectionID] = e
	}
	return e
}
