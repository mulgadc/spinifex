//test:in-package — walks the unexported ec2Actions route table.

package awsapi

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An action added to the route table with TagSpecifications must say which
// resource types it tags, or a wrong type would be silently dropped again.
func TestEC2TaggableTypes_CoverEveryAction(t *testing.T) {
	for action, handler := range ec2Actions {
		input, err := handler.parse(map[string]string{"Action": action})
		require.NoError(t, err, action)
		_, tags := reflect.TypeOf(input).Elem().FieldByName("TagSpecifications")
		_, listed := ec2TaggableTypes[action]
		assert.Equal(t, tags, listed, "%s: TagSpecifications modelled=%v, listed in ec2TaggableTypes=%v", action, tags, listed)
	}
	for action := range ec2TaggableTypes {
		assert.Contains(t, ec2Actions, action, "ec2TaggableTypes lists an action the registration does not route")
	}
}
