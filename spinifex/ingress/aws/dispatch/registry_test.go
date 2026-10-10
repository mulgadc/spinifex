package dispatch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func noopDispatch(http.ResponseWriter, Invocation) error { return nil }

func validReg(service string, ops ...string) Registration {
	if len(ops) == 0 {
		ops = []string{"Describe" + service}
	}
	return Registration{
		Service:   service,
		Dispatch:  noopDispatch,
		Errors:    ErrorEnvelopeJSON,
		Inventory: Inventory{Registered: ops},
	}
}

func TestRegisterRejectsAClaimedName(t *testing.T) {
	cases := []struct {
		name   string
		first  Registration
		second Registration
	}{
		{"same service", validReg("ecr"), validReg("ecr")},
		{"alias of earlier service", validReg("bedrock"), func() Registration {
			r := validReg("runtime")
			r.Aliases = []string{"bedrock"}
			return r
		}()},
		{"service already an alias", func() Registration {
			r := validReg("bedrock")
			r.Aliases = []string{"bedrock-runtime"}
			return r
		}(), validReg("bedrock-runtime")},
		{"same inventory name", func() Registration {
			r := validReg("elasticloadbalancing")
			r.InventoryName = "elbv2"
			return r
		}(), func() Registration {
			r := validReg("elb")
			r.InventoryName = "elbv2"
			return r
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBuilder()
			require.NoError(t, b.Register(tc.first))
			require.Error(t, b.Register(tc.second))

			e, ok := b.Build().Lookup(tc.second.Service)
			if ok {
				assert.Equal(t, tc.first.Service, e.Service(), "the earlier claimant keeps the name")
			}
		})
	}
}

func TestRegisterRejectsAnInvalidRegistration(t *testing.T) {
	mutate := map[string]func(*Registration){
		"empty service":         func(r *Registration) { r.Service = "" },
		"uppercase service":     func(r *Registration) { r.Service = "ECR" },
		"bad alias":             func(r *Registration) { r.Aliases = []string{"bad alias"} },
		"alias repeats service": func(r *Registration) { r.Aliases = []string{r.Service} },
		"alias repeated":        func(r *Registration) { r.Aliases = []string{"x", "x"} },
		"bad inventory name":    func(r *Registration) { r.InventoryName = "Bad" },
		"nil dispatcher":        func(r *Registration) { r.Dispatch = nil },
		"unset envelope":        func(r *Registration) { r.Errors = ErrorEnvelopeUnset },
		"unknown envelope":      func(r *Registration) { r.Errors = ErrorEnvelope(99) },
		"empty inventory":       func(r *Registration) { r.Inventory = Inventory{} },
		"repeated operation":    func(r *Registration) { r.Inventory.Registered = []string{"A", "A"} },
		"empty operation":       func(r *Registration) { r.Inventory.Registered = []string{""} },
		"unregistered stub":     func(r *Registration) { r.Inventory.Stubbed = []string{"Other"} },
		"unregistered unsupported": func(r *Registration) {
			r.Inventory.Unsupported = []string{"Other"}
		},
		"stubbed and unsupported": func(r *Registration) {
			r.Inventory = Inventory{Registered: []string{"A"}, Stubbed: []string{"A"}, Unsupported: []string{"A"}}
		},
	}
	for name, m := range mutate {
		t.Run(name, func(t *testing.T) {
			r := validReg("ecr", "A", "B")
			m(&r)
			b := NewBuilder()
			require.Error(t, b.Register(r))
			assert.Empty(t, b.Build().Inventory(), "nothing from a rejected registration may be kept")
		})
	}
}

func TestRegisterFailsAfterBuild(t *testing.T) {
	b := NewBuilder()
	require.NoError(t, b.Register(validReg("ecr")))
	reg := b.Build()

	require.ErrorIs(t, b.Register(validReg("acm")), ErrSealed)
	_, ok := reg.Lookup("acm")
	assert.False(t, ok)
}

func TestLookupServesEveryNameOfOneRegistration(t *testing.T) {
	var got Invocation
	r := validReg("bedrock")
	r.Aliases = []string{"bedrock-runtime", "bedrock-agent"}
	r.Dispatch = func(_ http.ResponseWriter, inv Invocation) error {
		got = inv
		return errors.New(awserrors.ErrorValidationError)
	}
	b := NewBuilder()
	require.NoError(t, b.Register(r))
	reg := b.Build()

	for _, name := range []string{"bedrock", "bedrock-runtime", "bedrock-agent"} {
		e, ok := reg.Lookup(name)
		require.True(t, ok, name)
		assert.Equal(t, "bedrock", e.Service())
		assert.Equal(t, ErrorEnvelopeJSON, e.ErrorEnvelope())

		inv := Invocation{AccountID: "000000000001", Region: "ap-southeast-2"}
		err := e.Dispatch(httptest.NewRecorder(), inv)
		require.EqualError(t, err, awserrors.ErrorValidationError, "the dispatcher's error reaches the caller unchanged")
		assert.Equal(t, inv.AccountID, got.AccountID)
		assert.Equal(t, inv.Region, got.Region)
	}
}

func TestUnknownServiceKeepsTheEstablishedResult(t *testing.T) {
	b := NewBuilder()
	require.NoError(t, b.Register(validReg("ecr")))
	_, ok := b.Build().Lookup("route53")
	assert.False(t, ok)

	code, msg := UnknownService("route53")
	assert.Equal(t, awserrors.ErrorInvalidAction, code)
	assert.Equal(t, `Service "route53" is not served by this gateway.`, msg)
}

func TestResolveActionIsAdvisory(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Amz-Target", "Svc.DescribeThing")

	without := validReg("ecr")
	with := validReg("acm")
	with.ResolveAction = func(r *http.Request) string { return "DescribeThing" }
	b := NewBuilder()
	require.NoError(t, b.Register(without))
	require.NoError(t, b.Register(with))
	reg := b.Build()

	e, _ := reg.Lookup("ecr")
	assert.Empty(t, e.ResolveAction(req), "no resolver means the action is unknown, not an error")
	e, _ = reg.Lookup("acm")
	assert.Equal(t, "DescribeThing", e.ResolveAction(req))
}

func TestInventoryIsStableAndIndependentOfRegistrationOrder(t *testing.T) {
	regs := []Registration{
		{Service: "ecr", Dispatch: noopDispatch, Errors: ErrorEnvelopeJSON,
			Inventory: Inventory{Registered: []string{"Put", "Create", "Get"}, Stubbed: []string{"Put"}, Unsupported: []string{"Get"}}},
		{Service: "elasticloadbalancing", InventoryName: "elasticloadbalancingv2", Dispatch: noopDispatch, Errors: ErrorEnvelopeXML,
			Inventory: Inventory{Registered: []string{"Z", "A"}}},
		validReg("acm", "Request", "Describe"),
	}
	want := map[string]Inventory{
		"ecr":                    {Registered: []string{"Create", "Get", "Put"}, Stubbed: []string{"Put"}, Unsupported: []string{"Get"}},
		"elasticloadbalancingv2": {Registered: []string{"A", "Z"}},
		"acm":                    {Registered: []string{"Describe", "Request"}},
	}

	build := func(order []int) *Registry {
		b := NewBuilder()
		for _, i := range order {
			require.NoError(t, b.Register(regs[i]))
		}
		return b.Build()
	}
	for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}} {
		assert.Equal(t, want, build(order).Inventory(), "order %v", order)
	}

	reg := build([]int{0, 1, 2})
	snapshot := reg.Inventory()
	snapshot["ecr"].Registered[0] = "Mutated"
	assert.Equal(t, want, reg.Inventory(), "a caller's copy must not reach the registry")

	regs[0].Inventory.Registered[0] = "MutatedAfterRegister"
	assert.Equal(t, want, reg.Inventory(), "the registration's own slices must not reach the registry")
}

func TestRegistryAllowsConcurrentReads(t *testing.T) {
	b := NewBuilder()
	for _, s := range []string{"ecr", "acm", "eks"} {
		require.NoError(t, b.Register(validReg(s)))
	}
	reg := b.Build()

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 200 {
				e, ok := reg.Lookup("acm")
				if !ok || e.Service() != "acm" {
					t.Error("lookup lost a registration")
					return
				}
				if len(reg.Inventory()) != 3 {
					t.Error("inventory lost a registration")
					return
				}
			}
		})
	}
	wg.Wait()
}
