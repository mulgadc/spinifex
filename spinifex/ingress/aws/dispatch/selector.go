package dispatch

import (
	"fmt"
	"net/http"
	"slices"
)

// Legacy answers service selection for signed services not yet registered,
// so the selector can make one decision across both dispatch paths.
type Legacy interface {
	// Serves reports whether the legacy path dispatches service.
	Serves(service string) bool
	// ResolveAction returns the advisory action label for r, or "" if unknown.
	ResolveAction(r *http.Request, service string) string
	// JSONErrors reports whether service's errors use the AWS JSON 1.1 envelope.
	JSONErrors(service string) bool
}

// Selector picks the registration or legacy path serving a credential-scope
// service. A registered service always wins. Safe for concurrent use when
// legacy is, since neither it nor the registry is mutated.
type Selector struct {
	registry *Registry
	legacy   Legacy
}

// NewSelector returns a selector over reg, which may be nil when nothing is
// registered, falling back to legacy, which may be nil when nothing is.
func NewSelector(reg *Registry, legacy Legacy) *Selector {
	if legacy == nil {
		legacy = noLegacy{}
	}
	return &Selector{registry: reg, legacy: legacy}
}

// Lookup returns the registration serving service, if any.
func (s *Selector) Lookup(service string) (*Entry, bool) {
	if s.registry == nil {
		return nil, false
	}
	return s.registry.Lookup(service)
}

// Served reports whether either dispatch path serves service.
func (s *Selector) Served(service string) bool {
	if _, ok := s.Lookup(service); ok {
		return true
	}
	return s.legacy.Serves(service)
}

// ResolveAction returns the advisory action label for r: the registration's
// own resolver when service is registered, otherwise the legacy resolver.
func (s *Selector) ResolveAction(r *http.Request, service string) string {
	if e, ok := s.Lookup(service); ok {
		return e.ResolveAction(r)
	}
	return s.legacy.ResolveAction(r, service)
}

// JSONErrors reports whether service's errors render in the AWS JSON 1.1
// envelope: a registration's declared envelope, else the legacy answer.
func (s *Selector) JSONErrors(service string) bool {
	if e, ok := s.Lookup(service); ok {
		return e.ErrorEnvelope() == ErrorEnvelopeJSON
	}
	return s.legacy.JSONErrors(service)
}

// Validate fails if any name the registry serves, alias included, is also
// served by the legacy path, so no service is reachable down both at once.
func (s *Selector) Validate() error {
	if s.registry == nil {
		return nil
	}
	names := make([]string, 0, len(s.registry.entries))
	for n := range s.registry.entries {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if s.legacy.Serves(n) {
			return fmt.Errorf("dispatch: service %q is registered and also served by the legacy dispatch table", n)
		}
	}
	return nil
}

type noLegacy struct{}

func (noLegacy) Serves(string) bool                         { return false }
func (noLegacy) ResolveAction(*http.Request, string) string { return "" }
func (noLegacy) JSONErrors(string) bool                     { return false }

var _ Legacy = noLegacy{}
