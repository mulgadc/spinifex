//test:in-package — drives the unexported resolveAction with the registration.

package awsapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRegistration_DeclaresTheEC2Service(t *testing.T) {
	reg := NewRegistration(Deps{})

	assert.Equal(t, ServiceName, reg.Service)
	require.NotNil(t, reg.ResolveAction)
	require.NotNil(t, reg.Dispatch)
	assert.NotNil(t, reg.Errors)
	assert.ElementsMatch(t, ActionNames(), reg.Inventory.Registered)
	assert.Empty(t, reg.Inventory.Stubbed)
	assert.Empty(t, reg.Inventory.Unsupported)

	b := dispatch.NewBuilder()
	require.NoError(t, b.Register(reg))
}

func TestResolveAction(t *testing.T) {
	post := func(body string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}

	assert.Equal(t, "DescribeVpcs", resolveAction(post("Action=DescribeVpcs&Version=2016-11-15")))
	assert.Empty(t, resolveAction(post("Version=2016-11-15")))

	// The body stays readable for the dispatcher after the action is resolved.
	r := post("Action=DescribeVpcs")
	require.Equal(t, "DescribeVpcs", resolveAction(r))
	assert.Equal(t, "DescribeVpcs", resolveAction(r))
}

func TestResolveAction_UnreadableBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", errReader{})
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	assert.Empty(t, resolveAction(r))
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, http.ErrBodyReadAfterClose }
