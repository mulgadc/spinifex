//go:build e2e

package harness

import (
	"sort"
	"sync"
	"testing"
)

// Profile bundles a tenant account's credentials with a pre-built AWSClient
// scoped to that account. Credentials are injected statically so a single test
// binary can drive many tenants concurrently without mutating ~/.aws/credentials.
type Profile struct {
	Name      string
	AccountID string
	Client    *AWSClient
	Info      AccountInfo
}

// AccountCarousel manages multiple Profile entries created via
// `spx admin account create`. Concurrent reads via Get/Names are safe;
// Add must not race with itself.
type AccountCarousel struct {
	mu       sync.RWMutex
	profiles map[string]*Profile
}

// NewAccountCarousel returns an empty carousel.
func NewAccountCarousel() *AccountCarousel {
	return &AccountCarousel{profiles: make(map[string]*Profile)}
}

// Add registers a new profile under name, returning it. Duplicate names
// overwrite. info.AccessKeyID and info.SecretAccessKey must be non-empty.
func (a *AccountCarousel) Add(t *testing.T, env *Env, name string, info AccountInfo) *Profile {
	t.Helper()
	client := NewAWSClientWithCreds(t, env, info.AccessKeyID, info.SecretAccessKey)
	p := &Profile{
		Name:      name,
		AccountID: info.AccountID,
		Client:    client,
		Info:      info,
	}
	a.mu.Lock()
	a.profiles[name] = p
	a.mu.Unlock()
	return p
}

// Get returns the named profile or nil if absent.
func (a *AccountCarousel) Get(name string) *Profile {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.profiles[name]
}

// Names returns registered profile names in sorted order.
func (a *AccountCarousel) Names() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.profiles))
	for k := range a.profiles {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
