//test:in-package — routes is unexported, and the table must cover exactly the
// actions the registration serves.

package awsapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// routePath fills a route pattern with placeholder segments. The tag routes
// take an ARN, which has to be escaped to stay one path segment.
func routePath(pattern string) string {
	arn := url.PathEscape("arn:aws:eks:us-east-1:111122223333:cluster/c")
	path := strings.ReplaceAll(pattern, "*", arn)
	for {
		start := strings.Index(path, "{")
		if start < 0 {
			return path
		}
		end := strings.Index(path[start:], "}")
		path = path[:start] + "x" + path[start+end+1:]
	}
}

// A parameter refusal must reach the wire under a code the operation declares
// in the EKS model, or SDK clients cannot match it to a typed error.
func TestRegistration_ParameterRefusalCodeIsDeclaredByOperation(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	sub, err := nc.Subscribe("eks.>", func(m *nats.Msg) {
		if m.Reply != "" {
			_ = m.Respond(awserrors.GenerateErrorPayload(awserrors.ErrorInvalidParameterValue))
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	model, err := awsmodel.Load(awsmodel.EKS)
	require.NoError(t, err)
	registration := NewRegistration(Deps{NATS: nc})

	for _, route := range routes {
		if IsInternalAction(route.Action) {
			continue
		}
		// The handler takes no input and the model declares no client error.
		if route.Action == "ListAccessPolicies" {
			continue
		}
		t.Run(route.Action, func(t *testing.T) {
			operation, ok := model.Operation(route.Action)
			require.True(t, ok, "%s is not an operation in the EKS model", route.Action)
			declared := make([]string, 0, len(operation.Errors))
			for _, ref := range operation.Errors {
				declared = append(declared, ref.Shape)
			}

			req := httptest.NewRequest(route.Method, routePath(route.Pattern), strings.NewReader(`{}`))
			inv := dispatch.Invocation{
				Request:   req,
				AccountID: "111122223333",
				Region:    "us-east-1",
				Authorize: func(string, string, []string, iampolicy.ConditionKeys) error { return nil },
				Actor: func() (dispatch.Actor, error) {
					return dispatch.Actor{AccountID: "111122223333", PrincipalType: "user", UserName: "alice"}, nil
				},
			}

			err := registration.Dispatch(httptest.NewRecorder(), inv)

			require.Error(t, err)
			code, ok := awserrors.ResolveErrorCode(err)
			require.True(t, ok, "unresolvable error code: %v", err)
			assert.Contains(t, declared, code,
				"%s %s refuses a parameter with %s, which the operation does not declare", route.Method, route.Pattern, code)
		})
	}
}

// The access operations declare InvalidRequestException, not a parameter code.
func TestEKSInvalidRequestCodeIsRegistered(t *testing.T) {
	assert.Equal(t, "InvalidRequestException", awserrors.ErrorEKSInvalidRequest)
	assert.Equal(t, http.StatusBadRequest, awserrors.HTTPStatusForError(awserrors.Errorf(awserrors.ErrorEKSInvalidRequest, "x")))
	assert.Equal(t, "BadRequestException", awserrors.ErrorEKSBadRequest)
	assert.Equal(t, http.StatusBadRequest, awserrors.HTTPStatusForError(awserrors.Errorf(awserrors.ErrorEKSBadRequest, "x")))
}
