package awsapi

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/internal/awsmodel"
	awsxml "github.com/mulgadc/spinifex/spinifex/foundation/aws/xml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQueryPayload_ResultElementMatchesModel checks every registered action
// carries an <ActionResult> element exactly when the IAM model gives the
// operation an output, using the SDK's zero-value output for that action.
func TestQueryPayload_ResultElementMatchesModel(t *testing.T) {
	model, err := awsmodel.Load(awsmodel.IAM)
	require.NoError(t, err)
	client := reflect.TypeFor[*iam.IAM]()

	for _, name := range ActionNames() {
		t.Run(name, func(t *testing.T) {
			op, ok := model.Operation(name)
			require.True(t, ok, "model has no operation %s", name)
			method, ok := client.MethodByName(name)
			require.True(t, ok, "SDK has no method %s", name)
			output := reflect.New(method.Type.Out(0).Elem()).Interface()

			body, err := awsxml.Marshal(queryPayload(name, output))
			require.NoError(t, err)
			xmlText := string(body)
			assert.True(t, strings.HasPrefix(xmlText, "<"+name+"Response>"), xmlText)
			assert.Equal(t, op.Output != nil, strings.Contains(xmlText, "<"+name+"Result"), xmlText)
		})
	}
}
