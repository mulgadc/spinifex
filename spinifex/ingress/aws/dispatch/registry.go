// Package dispatch is the AWS ingress registration contract. Each signed AWS
// service registers one dispatcher during role startup, and generic ingress
// selects it by SigV4 credential-scope service once the caller is authenticated.
package dispatch

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// ErrorEnvelope selects which generic error rendering a service's errors use.
type ErrorEnvelope int

const (
	// ErrorEnvelopeUnset is invalid; a registration must choose an envelope.
	ErrorEnvelopeUnset ErrorEnvelope = iota
	// ErrorEnvelopeXML is the AWS Query/XML error body.
	ErrorEnvelopeXML
	// ErrorEnvelopeJSON is the AWS JSON 1.1 error body with X-Amzn-Errortype.
	ErrorEnvelopeJSON
)

// Inventory classifies a service's dispatched operations. Registered minus
// Stubbed and Unsupported are implemented; Stubbed and Unsupported are
// disjoint subsets of Registered.
type Inventory struct {
	Registered  []string
	Stubbed     []string
	Unsupported []string
}

// Actor is the verified caller as ingress authenticated it. It carries only
// the facts a service needs to act for the caller, never credentials.
type Actor struct {
	AccountID     string
	CallerARN     string
	PrincipalType string
	AccessKeyID   string
}

// AuthorizeFunc asks ingress for an IAM decision on the authenticated caller.
// A nil error permits the operation; otherwise the error is the AWS result.
type AuthorizeFunc func(service, action string, resources []string, keys iampolicy.ConditionKeys) error

// ActorFunc resolves the verified caller on demand, so a request that never
// needs it cannot fail on it.
type ActorFunc func() (Actor, error)

// Invocation is an authenticated request handed to a service dispatcher.
// Request's body is bounded and rewindable; its context carries the trace.
type Invocation struct {
	Request   *http.Request
	AccountID string
	Region    string
	Authorize AuthorizeFunc
	Actor     ActorFunc
}

// Dispatcher parses, authorizes and runs one request. It writes the success
// response itself; a returned error is rendered in the registration's envelope.
type Dispatcher func(w http.ResponseWriter, inv Invocation) error

// ActionResolver labels a request with its AWS action for throttling, tracing
// and audit. It is advisory: "" means unknown, and dispatch stays authoritative.
type ActionResolver func(r *http.Request) string

// Registration declares one signed AWS service.
type Registration struct {
	// Service is the SigV4 credential-scope name; Aliases are further names
	// served by the same dispatcher.
	Service string
	Aliases []string
	// InventoryName keys the operation inventory; it defaults to Service.
	InventoryName string
	ResolveAction ActionResolver
	Dispatch      Dispatcher
	Errors        ErrorEnvelope
	Inventory     Inventory
}

// UnknownService returns the client-visible code and message for a credential
// scope no registration serves. Nothing was wrong with the credentials, so it
// is an action error rather than an authentication one.
func UnknownService(service string) (code, message string) {
	return awserrors.ErrorInvalidAction, fmt.Sprintf("Service %q is not served by this gateway.", service)
}

// ErrSealed is returned by Register once Build has produced the registry.
var ErrSealed = errors.New("dispatch: registry already built")

var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Builder collects registrations during startup. It is not safe for
// concurrent use; Build returns the immutable registry the server reads.
type Builder struct {
	entries   map[string]*Entry
	inventory map[string]Inventory
	sealed    bool
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder {
	return &Builder{
		entries:   make(map[string]*Entry),
		inventory: make(map[string]Inventory),
	}
}

// Register validates reg and adds it, failing on any invalid field or on a
// service, alias or inventory name another registration already claims.
func (b *Builder) Register(reg Registration) error {
	if b.sealed {
		return ErrSealed
	}
	names, err := validate(reg)
	if err != nil {
		return err
	}
	for _, n := range names {
		if other, ok := b.entries[n]; ok {
			return fmt.Errorf("dispatch: service %q is already registered by %q", n, other.service)
		}
	}
	invName := reg.InventoryName
	if invName == "" {
		invName = reg.Service
	}
	if _, ok := b.inventory[invName]; ok {
		return fmt.Errorf("dispatch: inventory %q is already registered", invName)
	}

	e := &Entry{
		service: reg.Service,
		resolve: reg.ResolveAction,
		handle:  reg.Dispatch,
		errors:  reg.Errors,
	}
	for _, n := range names {
		b.entries[n] = e
	}
	b.inventory[invName] = sortedInventory(reg.Inventory)
	return nil
}

// Build seals the builder and returns the registry. Further Register calls fail.
func (b *Builder) Build() *Registry {
	b.sealed = true
	return &Registry{
		entries:   maps.Clone(b.entries),
		inventory: maps.Clone(b.inventory),
	}
}

func validate(reg Registration) ([]string, error) {
	if !serviceNamePattern.MatchString(reg.Service) {
		return nil, fmt.Errorf("dispatch: invalid service name %q", reg.Service)
	}
	names := []string{reg.Service}
	for _, a := range reg.Aliases {
		if !serviceNamePattern.MatchString(a) {
			return nil, fmt.Errorf("dispatch: %s: invalid alias %q", reg.Service, a)
		}
		if slices.Contains(names, a) {
			return nil, fmt.Errorf("dispatch: %s: alias %q repeats a name", reg.Service, a)
		}
		names = append(names, a)
	}
	if reg.InventoryName != "" && !serviceNamePattern.MatchString(reg.InventoryName) {
		return nil, fmt.Errorf("dispatch: %s: invalid inventory name %q", reg.Service, reg.InventoryName)
	}
	if reg.Dispatch == nil {
		return nil, fmt.Errorf("dispatch: %s: nil dispatcher", reg.Service)
	}
	if reg.Errors != ErrorEnvelopeXML && reg.Errors != ErrorEnvelopeJSON {
		return nil, fmt.Errorf("dispatch: %s: error envelope not set", reg.Service)
	}
	if err := validateInventory(reg.Inventory); err != nil {
		return nil, fmt.Errorf("dispatch: %s: %w", reg.Service, err)
	}
	return names, nil
}

func validateInventory(inv Inventory) error {
	if len(inv.Registered) == 0 {
		return errors.New("inventory has no registered operations")
	}
	registered := make(map[string]bool, len(inv.Registered))
	for _, op := range inv.Registered {
		if op == "" || registered[op] {
			return fmt.Errorf("registered operation %q is empty or repeated", op)
		}
		registered[op] = true
	}
	stubbed := make(map[string]bool, len(inv.Stubbed))
	for _, op := range inv.Stubbed {
		if !registered[op] || stubbed[op] {
			return fmt.Errorf("stubbed operation %q is unregistered or repeated", op)
		}
		stubbed[op] = true
	}
	unsupported := make(map[string]bool, len(inv.Unsupported))
	for _, op := range inv.Unsupported {
		if !registered[op] || unsupported[op] || stubbed[op] {
			return fmt.Errorf("unsupported operation %q is unregistered, repeated or also stubbed", op)
		}
		unsupported[op] = true
	}
	return nil
}

func sortedInventory(inv Inventory) Inventory {
	return Inventory{
		Registered:  sortedClone(inv.Registered),
		Stubbed:     sortedClone(inv.Stubbed),
		Unsupported: sortedClone(inv.Unsupported),
	}
}

func sortedClone(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

// Registry is the immutable set of signed-service registrations. It is safe
// for concurrent use because nothing mutates it after Build.
type Registry struct {
	entries   map[string]*Entry
	inventory map[string]Inventory
}

// Lookup returns the registration serving the credential-scope service.
func (r *Registry) Lookup(service string) (*Entry, bool) {
	e, ok := r.entries[service]
	return e, ok
}

// Inventory returns a fresh copy of every registration's operation inventory,
// keyed by inventory name with each list sorted.
func (r *Registry) Inventory() map[string]Inventory {
	out := make(map[string]Inventory, len(r.inventory))
	for name, inv := range r.inventory {
		out[name] = Inventory{
			Registered:  slices.Clone(inv.Registered),
			Stubbed:     slices.Clone(inv.Stubbed),
			Unsupported: slices.Clone(inv.Unsupported),
		}
	}
	return out
}

// Entry is one registration as the server reads it.
type Entry struct {
	service string
	resolve ActionResolver
	handle  Dispatcher
	errors  ErrorEnvelope
}

// Service returns the registration's primary credential-scope name.
func (e *Entry) Service() string { return e.service }

// ErrorEnvelope returns the envelope the service's errors render in.
func (e *Entry) ErrorEnvelope() ErrorEnvelope { return e.errors }

// ResolveAction returns the advisory action label for r, or "" when the
// registration has no resolver or cannot name the action.
func (e *Entry) ResolveAction(r *http.Request) string {
	if e.resolve == nil {
		return ""
	}
	return e.resolve(r)
}

// Dispatch hands an authenticated invocation to the service.
func (e *Entry) Dispatch(w http.ResponseWriter, inv Invocation) error {
	return e.handle(w, inv)
}
