package awsapi

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A value outside a List* enum is refused before dispatch with AWS's message.
// A valid one reaches dispatch, which fails here only because nc is nil.
func TestListEnums(t *testing.T) {
	cases := []struct {
		name    string
		handler describeHandler
		member  string
		valid   []string
		wantMsg string
	}{
		{"ListServices", ListServices, "launchType", []string{"EC2", "FARGATE", "EXTERNAL", "FMI", "MANAGED_INSTANCES"},
			"launch type should be one of [EC2,FARGATE,EXTERNAL,FMI,MANAGED_INSTANCES]"},
		{"ListServices", ListServices, "schedulingStrategy", []string{"REPLICA", "DAEMON"},
			"Scheduling strategy should be one of [REPLICA,DAEMON]"},
		{"ListServices", ListServices, "resourceManagementType", []string{"CUSTOMER", "ECS"},
			"Resource management type should be one of [CUSTOMER,ECS]"},
		{"ListTaskDefinitions", ListTaskDefinitions, "sort", []string{"ASC", "DESC"},
			"Sort should be one of [ASC,DESC]"},
		{"ListTaskDefinitions", ListTaskDefinitions, "status", []string{"ACTIVE", "INACTIVE", "DELETE_IN_PROGRESS"},
			"Status should be one of [ACTIVE,INACTIVE,DELETE_IN_PROGRESS]"},
		{"ListTasks", ListTasks, "desiredStatus", []string{"RUNNING", "PENDING", "STOPPED"},
			"Desired Status should be one of [RUNNING,PENDING,STOPPED]"},
		{"ListTasks", ListTasks, "launchType", []string{"EC2", "FARGATE", "EXTERNAL", "FMI", "MANAGED_INSTANCES"},
			"launch type should be one of [EC2,FARGATE,EXTERNAL,FMI,MANAGED_INSTANCES]"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/"+tc.member+"/invalid", func(t *testing.T) {
			_, err := tc.handler(context.Background(), nil, "123456789012", []byte(`{"`+tc.member+`":"BOGUS"}`), nil)
			code, msg, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok, "err = %v", err)
			assert.Equal(t, awserrors.ErrorECSInvalidParameter, code)
			assert.Equal(t, tc.wantMsg, msg)
		})
		for _, v := range tc.valid {
			t.Run(tc.name+"/"+tc.member+"/"+v, func(t *testing.T) {
				_, err := tc.handler(context.Background(), nil, "123456789012", []byte(`{"`+tc.member+`":"`+v+`"}`), nil)
				_, _, refused := awserrors.ResolveErrorDetail(err)
				assert.False(t, refused, "err = %v", err)
			})
		}
	}
}
