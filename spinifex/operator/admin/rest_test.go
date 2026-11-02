package admin

import (
	"testing"

	operatorv1 "github.com/mulgadc/spinifex/contracts/operator/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every method in the contract list must have a handler, or the route accepts
// the name and then answers InvalidAction on a cluster that looks configured.
func TestRunMethod_HandlesEveryContractMethod(t *testing.T) {
	for _, method := range operatorv1.AdminMethodNames() {
		t.Run(method, func(t *testing.T) {
			_, err := Deps{}.RunMethod(t.Context(), method, dispatch.Actor{}, []byte(`{`))

			require.Error(t, err)
			code, ok := awserrors.ResolveErrorCode(err)
			require.True(t, ok)
			assert.NotEqual(t, awserrors.ErrorInvalidAction, code)
		})
	}
}

func TestRunMethod_UnknownMethodFailsClosed(t *testing.T) {
	_, err := Deps{}.RunMethod(t.Context(), "NoSuchMethod", dispatch.Actor{}, nil)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}
