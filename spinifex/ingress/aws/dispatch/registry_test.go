package dispatch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
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
	_, ok := b.Build().Lookup("sqs")
	assert.False(t, ok)

	code, msg := UnknownService("sqs")
	assert.Equal(t, awserrors.ErrorInvalidAction, code)
	assert.Equal(t, `Service "sqs" is not served by this gateway.`, msg)
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

func TestRegisterValidatesErrorSpecs(t *testing.T) {
	spec := &ErrorSpec{Code: "Throttled", Status: 429}
	mutate := map[string]func(*Registration){
		"throttle on XML":         func(r *Registration) { r.Errors = ErrorEnvelopeXML; r.Throttle = spec },
		"throttle without code":   func(r *Registration) { r.Throttle = &ErrorSpec{Status: 429} },
		"throttle with 2xx":       func(r *Registration) { r.Throttle = &ErrorSpec{Code: "X", Status: 200} },
		"query codes on JSON 1.1": func(r *Registration) { r.QueryErrorCodes = map[string]string{"A": "B"} },
		"empty query code": func(r *Registration) {
			r.Errors = ErrorEnvelopeJSON10Query
			r.QueryErrorCodes = map[string]string{"A": ""}
		},
	}
	for name, m := range mutate {
		t.Run(name, func(t *testing.T) {
			r := validReg("ecr")
			m(&r)
			require.Error(t, NewBuilder().Register(r))
		})
	}

	for _, env := range []ErrorEnvelope{ErrorEnvelopeRESTJSON, ErrorEnvelopeJSON10Query} {
		r := validReg("svc")
		r.Errors = env
		r.Throttle = spec
		require.NoError(t, NewBuilder().Register(r))
	}
}

func TestEntryThrottleErrorDefaultsPerEnvelope(t *testing.T) {
	entry := func(env ErrorEnvelope) *Entry {
		b := NewBuilder()
		r := validReg("svc")
		r.Errors = env
		require.NoError(t, b.Register(r))
		e, ok := b.Build().Lookup("svc")
		require.True(t, ok)
		return e
	}

	_, own := entry(ErrorEnvelopeJSON).ThrottleError()
	assert.False(t, own, "JSON 1.1 keeps the gateway default")
	_, own = entry(ErrorEnvelopeXML).ThrottleError()
	assert.False(t, own, "XML keeps the gateway default")

	rest, own := entry(ErrorEnvelopeRESTJSON).ThrottleError()
	require.True(t, own)
	assert.Equal(t, ErrorSpec{
		Code: "TooManyRequestsException", Message: "Rate Exceeded.", Status: 429,
		Fields: map[string]string{"Reason": "CallerRateLimitExceeded"},
	}, rest)
	rest.Fields["Reason"] = "mutated"
	again, _ := entry(ErrorEnvelopeRESTJSON).ThrottleError()
	assert.Equal(t, "CallerRateLimitExceeded", again.Fields["Reason"], "a caller cannot mutate the default")

	q, own := entry(ErrorEnvelopeJSON10Query).ThrottleError()
	require.True(t, own)
	assert.Equal(t, "RequestThrottled", q.Code)
	assert.Equal(t, 400, q.Status)
}

func TestSelectorErrorProtocol(t *testing.T) {
	b := NewBuilder()
	for svc, env := range map[string]ErrorEnvelope{
		"rest": ErrorEnvelopeRESTJSON, "q": ErrorEnvelopeJSON10Query, "j": ErrorEnvelopeJSON, "x": ErrorEnvelopeXML,
	} {
		r := validReg(svc)
		r.Errors = env
		require.NoError(t, b.Register(r))
	}
	sel := NewSelector(b.Build(), nil)

	for svc, want := range map[string]envelope.Protocol{
		"rest": envelope.ProtocolRESTJSON, "q": envelope.ProtocolJSON10Query, "j": envelope.ProtocolJSON11,
	} {
		p, ok := sel.ErrorProtocol(svc)
		assert.True(t, ok, svc)
		assert.Equal(t, want, p, svc)
		assert.True(t, sel.JSONErrors(svc), svc)
	}
	_, ok := sel.ErrorProtocol("x")
	assert.False(t, ok)
	assert.False(t, sel.JSONErrors("x"))
	_, ok = sel.ErrorProtocol("unknown")
	assert.False(t, ok)
}

func TestEntryQueryErrorCode(t *testing.T) {
	b := NewBuilder()
	r := validReg("svc")
	r.Errors = ErrorEnvelopeJSON10Query
	r.QueryErrorCodes = map[string]string{"QueueDoesNotExist": "AWS.SimpleQueueService.NonExistentQueue"}
	require.NoError(t, b.Register(r))
	e, _ := b.Build().Lookup("svc")
	assert.Equal(t, "AWS.SimpleQueueService.NonExistentQueue", e.QueryErrorCode("QueueDoesNotExist"))
	assert.Equal(t, "OverLimit", e.QueryErrorCode("OverLimit"))
}

func TestEntryInternalErrorCode(t *testing.T) {
	entry := func(env ErrorEnvelope, code string) *Entry {
		b := NewBuilder()
		r := validReg("svc")
		r.Errors = env
		r.InternalErrorCode = code
		require.NoError(t, b.Register(r))
		e, _ := b.Build().Lookup("svc")
		return e
	}
	code, ok := entry(ErrorEnvelopeRESTJSON, "").InternalErrorCode()
	assert.True(t, ok)
	assert.Equal(t, "ServiceException", code)
	code, ok = entry(ErrorEnvelopeJSON10Query, "").InternalErrorCode()
	assert.True(t, ok)
	assert.Equal(t, "InternalFailure", code)
	code, _ = entry(ErrorEnvelopeRESTJSON, "Mine").InternalErrorCode()
	assert.Equal(t, "Mine", code)
	_, ok = entry(ErrorEnvelopeJSON, "").InternalErrorCode()
	assert.False(t, ok)
	_, ok = entry(ErrorEnvelopeXML, "").InternalErrorCode()
	assert.False(t, ok)

	r := validReg("svc")
	r.InternalErrorCode = "Mine"
	require.Error(t, NewBuilder().Register(r), "JSON 1.1 cannot set an internal error code")
}
