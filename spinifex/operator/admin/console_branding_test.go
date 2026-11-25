//test:in-package — drives dispatchREST and authorizeREST, which are unexported.
package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/operator/consolebranding"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var brandingMethods = []string{"GetConsoleBranding", "PutConsoleBranding"}

func brandingDeps(t *testing.T, enabled bool) (Deps, *nats.Conn) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := consolebranding.OpenBucket(t.Context(), js)
	require.NoError(t, err)
	return Deps{NATS: nc, Branding: &ConsoleBranding{
		Enabled: enabled,
		Store:   consolebranding.Store{KV: kv},
		Cache:   &consolebranding.Cache{},
		Now:     func() time.Time { return time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC) },
	}}, nc
}

func runBranding(t *testing.T, d Deps, method string, body any) (operatorv1.ConsoleBrandingResponse, error) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	out, err := d.RunMethod(t.Context(), method, adminActor(), raw)
	if err != nil {
		return operatorv1.ConsoleBrandingResponse{}, err
	}
	resp, ok := out.(operatorv1.ConsoleBrandingResponse)
	require.True(t, ok, "unexpected response type %T", out)
	return resp, nil
}

func errorCode(t *testing.T, err error) string {
	t.Helper()
	require.Error(t, err)
	code, _ := awserrors.ResolveErrorCode(err)
	return code
}

func TestConsoleBranding_GateOff(t *testing.T) {
	d, _ := brandingDeps(t, false)

	_, err := runBranding(t, d, "PutConsoleBranding", operatorv1.PutConsoleBrandingRequest{ProductName: "Acme"})
	assert.Equal(t, awserrors.ErrorUnsupportedOperation, errorCode(t, err))
	_, message, _ := awserrors.ResolveErrorDetail(err)
	assert.Contains(t, message, "branding_enabled")

	got, err := runBranding(t, d, "GetConsoleBranding", struct{}{})
	require.NoError(t, err, "an operator can read what is stored with the gate off")
	assert.False(t, got.Enabled)
}

// Turning the gate off and on again keeps the stored record.
func TestConsoleBranding_RecordSurvivesGateOffAndOn(t *testing.T) {
	d, _ := brandingDeps(t, true)
	_, err := runBranding(t, d, "PutConsoleBranding", operatorv1.PutConsoleBrandingRequest{ProductName: "Acme"})
	require.NoError(t, err)

	d.Branding.Enabled = false
	off, err := runBranding(t, d, "GetConsoleBranding", struct{}{})
	require.NoError(t, err)
	assert.Equal(t, "Acme", off.ProductName)

	d.Branding.Enabled = true
	on, err := runBranding(t, d, "GetConsoleBranding", struct{}{})
	require.NoError(t, err)
	assert.Equal(t, "Acme", on.ProductName)
	assert.True(t, on.Enabled)
}

func TestConsoleBranding_SaveReadAndReset(t *testing.T) {
	d, _ := brandingDeps(t, true)
	logo, err := json.Marshal(operatorv1.BrandingImage{ContentType: consolebranding.TypeSVG, Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)})
	require.NoError(t, err)

	saved, err := runBranding(t, d, "PutConsoleBranding", operatorv1.PutConsoleBrandingRequest{
		ProductName: "Acme Cloud",
		Primary:     operatorv1.BrandingColors{Light: "#0A4A8F"},
		Logo:        logo,
	})
	require.NoError(t, err)
	assert.Equal(t, "Acme Cloud", saved.ProductName)
	assert.Equal(t, &operatorv1.BrandingImageInfo{ContentType: consolebranding.TypeSVG, Size: 41}, saved.Logo)
	assert.Equal(t, "signup", saved.UpdatedBy)
	assert.Equal(t, "2026-11-25T10:00:00Z", saved.UpdatedAt)

	rec, rev := d.Branding.Cache.Snapshot()
	require.NotNil(t, rec, "a save updates this gateway's copy at once")
	assert.Equal(t, saved.Revision, rev)

	read, err := runBranding(t, d, "GetConsoleBranding", struct{}{})
	require.NoError(t, err)
	assert.Equal(t, saved, read)

	reset, err := runBranding(t, d, "PutConsoleBranding", map[string]any{
		"revision": read.Revision, "logo": nil, "logoDark": nil, "favicon": nil,
	})
	require.NoError(t, err)
	assert.Empty(t, reset.ProductName)
	assert.Nil(t, reset.Logo)
	assert.Equal(t, operatorv1.BrandingColors{}, reset.Primary)
	assert.Greater(t, reset.Revision, read.Revision, "a reset is a new revision, so every asset URL changes")
}

func TestConsoleBranding_StaleRevisionLeavesRecordUnchanged(t *testing.T) {
	d, _ := brandingDeps(t, true)
	first, err := runBranding(t, d, "PutConsoleBranding", operatorv1.PutConsoleBrandingRequest{ProductName: "First"})
	require.NoError(t, err)
	_, err = runBranding(t, d, "PutConsoleBranding", operatorv1.PutConsoleBrandingRequest{Revision: first.Revision, ProductName: "Second"})
	require.NoError(t, err)

	_, err = runBranding(t, d, "PutConsoleBranding", operatorv1.PutConsoleBrandingRequest{Revision: first.Revision, ProductName: "Lost"})
	assert.Equal(t, awserrors.ErrorPreconditionFailedException, errorCode(t, err))

	got, err := runBranding(t, d, "GetConsoleBranding", struct{}{})
	require.NoError(t, err)
	assert.Equal(t, "Second", got.ProductName)
}

func TestConsoleBranding_InvalidFieldIsNamed(t *testing.T) {
	d, _ := brandingDeps(t, true)

	_, err := runBranding(t, d, "PutConsoleBranding", operatorv1.PutConsoleBrandingRequest{Primary: operatorv1.BrandingColors{Light: "#0073EE"}})

	assert.Equal(t, awserrors.ErrorInvalidParameterValue, errorCode(t, err))
	_, message, _ := awserrors.ResolveErrorDetail(err)
	assert.Contains(t, message, "primary.light")
	assert.Contains(t, message, "button text")
}

// Every identity gate denies both methods with the same AccessDenied.
func TestConsoleBranding_OnlyTheOperatorIsAuthorized(t *testing.T) {
	with := func(mod func(*dispatch.Actor)) dispatch.Actor {
		a := adminActor()
		mod(&a)
		return a
	}
	callers := map[string]dispatch.Actor{
		"tenant account": with(func(a *dispatch.Actor) { a.AccountID = "123456789012" }),
		"assumed-role session": with(func(a *dispatch.Actor) {
			a.PrincipalType = dispatch.PrincipalTypeAssumedRole
			a.SessionName = "signup"
		}),
	}
	for _, method := range brandingMethods {
		for name, caller := range callers {
			t.Run(method+"/"+name, func(t *testing.T) {
				inv, _ := restInvocation(http.MethodPost, "/admin/"+method, "{}", allowAll, ServiceName)
				assert.Equal(t, awserrors.ErrorAccessDenied, errorCode(t, Deps{}.authorizeREST(inv, method, caller)))
			})
		}
	}
}

// principalAuthorize evaluates the inline policy spx admin principal create
// writes for the given grants.
func principalAuthorize(t *testing.T, methods []string) dispatch.AuthorizeFunc {
	t.Helper()
	actions := make([]string, 0, len(methods))
	for _, m := range methods {
		actions = append(actions, fmt.Sprintf("%q", ServiceName+":"+m))
	}
	var doc iampolicy.PolicyDocument
	require.NoError(t, json.Unmarshal([]byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":[`+
		strings.Join(actions, ",")+`],"Resource":"*"}]}`), &doc))
	return func(service, action string, resources []string, keys iampolicy.ConditionKeys) error {
		if iampolicy.EvaluateWithKeys(service+":"+action, resources[0], []iampolicy.PolicyDocument{doc}, keys) != iampolicy.Allow {
			return awserrors.Errorf(awserrors.ErrorAccessDenied, "denied")
		}
		return nil
	}
}

// Grants stay explicit: a principal minted before the methods existed holds
// the method list of that day and is denied; one minted with them succeeds.
func TestConsoleBranding_PrincipalGrantsAreExplicit(t *testing.T) {
	before := slices.DeleteFunc(operatorv1.AdminMethodNames(), func(m string) bool { return slices.Contains(brandingMethods, m) })
	for _, method := range brandingMethods {
		inv, _ := restInvocation(http.MethodPost, "/admin/"+method, "{}", principalAuthorize(t, before), ServiceName)
		assert.Equal(t, awserrors.ErrorAccessDenied, errorCode(t, Deps{}.authorizeREST(inv, method, adminActor())), method)

		inv, _ = restInvocation(http.MethodPost, "/admin/"+method, "{}", principalAuthorize(t, brandingMethods), ServiceName)
		assert.NoError(t, Deps{}.authorizeREST(inv, method, adminActor()), method)
	}
}

func TestDispatchREST_BodyLimitIsPerMethod(t *testing.T) {
	d, _ := brandingDeps(t, true)
	tests := []struct {
		method string
		size   int
		want   string
	}{
		{"PutConsoleBranding", 720<<10 + 1, "exceeds 720 KiB"},
		{"GetConsoleBranding", 8<<10 + 1, "exceeds 8 KiB"},
		{"ListAccounts", 8<<10 + 1, "exceeds 8 KiB"},
	}
	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			body := `{"productName":"` + strings.Repeat("a", tc.size) + `"}`
			inv, w := restInvocation(http.MethodPost, "/admin/"+tc.method, body, allowAll, ServiceName)
			err := d.dispatchREST(w, inv)
			assert.Equal(t, awserrors.ErrorInvalidRequest, errorCode(t, err))
			_, message, _ := awserrors.ResolveErrorDetail(err)
			assert.Contains(t, message, tc.want)
		})
	}

	// A body inside the larger cap reaches the method, which judges its content.
	body := `{"productName":"` + strings.Repeat("a", 100<<10) + `"}`
	inv, w := restInvocation(http.MethodPost, "/admin/PutConsoleBranding", body, allowAll, ServiceName)
	err := d.dispatchREST(w, inv)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, errorCode(t, err))
}
