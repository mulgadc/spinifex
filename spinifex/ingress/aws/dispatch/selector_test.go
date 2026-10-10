package dispatch

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLegacy serves a fixed set of names, labels every request "LegacyAction"
// and renders JSON errors only for the names in json.
type fakeLegacy struct {
	served map[string]bool
	json   map[string]bool
}

func (f fakeLegacy) Serves(svc string) bool { return f.served[svc] }

func (f fakeLegacy) ResolveAction(*http.Request, string) string { return "LegacyAction" }

func (f fakeLegacy) JSONErrors(svc string) bool { return f.json[svc] }

var _ Legacy = fakeLegacy{}

func newFakeLegacy(served ...string) fakeLegacy {
	f := fakeLegacy{served: map[string]bool{}, json: map[string]bool{}}
	for _, s := range served {
		f.served[s] = true
	}
	return f
}

// selectorRegistry registers "ecr" (XML envelope, resolver "RegisteredAction",
// alias "ecr-alt") and "noresolver" (JSON envelope, no resolver).
func selectorRegistry(t *testing.T) *Registry {
	t.Helper()
	b := NewBuilder()
	ecr := validReg("ecr")
	ecr.Aliases = []string{"ecr-alt"}
	ecr.InventoryName = "ecrinv"
	ecr.Errors = ErrorEnvelopeXML
	ecr.ResolveAction = func(*http.Request) string { return "RegisteredAction" }
	require.NoError(t, b.Register(ecr))
	require.NoError(t, b.Register(validReg("noresolver")))
	return b.Build()
}

func TestSelectorRegisteredWins(t *testing.T) {
	// The legacy side claims every name and answers the opposite envelope, so
	// any fall-through to it on a registered name is visible.
	legacy := newFakeLegacy()
	legacy.json["ecr"] = true
	legacy.json["ecr-alt"] = true
	s := NewSelector(selectorRegistry(t), legacy)
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	for _, name := range []string{"ecr", "ecr-alt"} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, s.Served(name))
			assert.Equal(t, "RegisteredAction", s.ResolveAction(req, name))
			assert.False(t, s.JSONErrors(name), "registered XML envelope must beat legacy JSON")
			e, ok := s.Lookup(name)
			require.True(t, ok)
			assert.Equal(t, "ecr", e.Service())
		})
	}

	t.Run("registered without resolver stays unlabelled", func(t *testing.T) {
		assert.Empty(t, s.ResolveAction(req, "noresolver"), "must not fall back to the legacy resolver")
		assert.True(t, s.JSONErrors("noresolver"))
	})

	t.Run("inventory name is not a service", func(t *testing.T) {
		assert.False(t, s.Served("ecrinv"))
		_, ok := s.Lookup("ecrinv")
		assert.False(t, ok)
	})
}

func TestSelectorLegacyFallback(t *testing.T) {
	legacy := newFakeLegacy("ec2", "acm")
	legacy.json["acm"] = true
	s := NewSelector(selectorRegistry(t), legacy)
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	assert.True(t, s.Served("ec2"))
	assert.True(t, s.Served("acm"))
	assert.False(t, s.Served("route53"))
	assert.Equal(t, "LegacyAction", s.ResolveAction(req, "ec2"))
	assert.False(t, s.JSONErrors("ec2"))
	assert.True(t, s.JSONErrors("acm"))
	_, ok := s.Lookup("ec2")
	assert.False(t, ok)
}

func TestSelectorNilRegistry(t *testing.T) {
	legacy := newFakeLegacy("ec2")
	legacy.json["ec2"] = true
	s := NewSelector(nil, legacy)
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	_, ok := s.Lookup("ecr")
	assert.False(t, ok)
	assert.False(t, s.Served("ecr"))
	assert.True(t, s.Served("ec2"))
	assert.Equal(t, "LegacyAction", s.ResolveAction(req, "ec2"))
	assert.True(t, s.JSONErrors("ec2"))
	assert.NoError(t, s.Validate())
}

func TestSelectorNilLegacyServesOnlyRegistered(t *testing.T) {
	s := NewSelector(selectorRegistry(t), nil)
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	assert.True(t, s.Served("ecr"))
	assert.False(t, s.Served("ec2"))
	assert.Empty(t, s.ResolveAction(req, "ec2"))
	assert.False(t, s.JSONErrors("ec2"))
	assert.NoError(t, s.Validate())
}

func TestSelectorValidate(t *testing.T) {
	cases := []struct {
		name    string
		legacy  []string
		wantErr bool
	}{
		{"disjoint", []string{"ec2", "acm"}, false},
		{"primary name overlaps", []string{"ec2", "ecr"}, true},
		{"alias overlaps", []string{"ecr-alt"}, true},
		{"inventory name is not a served name", []string{"ecrinv"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NewSelector(selectorRegistry(t), newFakeLegacy(tc.legacy...)).Validate()
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}

	t.Run("empty registry", func(t *testing.T) {
		assert.NoError(t, NewSelector(NewBuilder().Build(), newFakeLegacy("ecr")).Validate())
	})
}

func TestSelectorConcurrentUse(t *testing.T) {
	legacy := newFakeLegacy("ec2")
	s := NewSelector(selectorRegistry(t), legacy)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			for range 100 {
				assert.True(t, s.Served("ecr"))
				assert.True(t, s.Served("ec2"))
				assert.Equal(t, "RegisteredAction", s.ResolveAction(req, "ecr"))
				assert.Equal(t, "LegacyAction", s.ResolveAction(req, "ec2"))
				assert.False(t, s.JSONErrors("ecr"))
				assert.NoError(t, s.Validate())
			}
		})
	}
	wg.Wait()
}
