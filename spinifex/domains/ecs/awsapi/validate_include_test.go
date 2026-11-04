package awsapi

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type describeHandler func(context.Context, *nats.Conn, string, []byte, PassRoleChecker) (any, error)

// A refused include answers before dispatch with AWS's code and message. A
// valid one reaches dispatch, which fails here only because nc is nil.
func TestDescribeInclude(t *testing.T) {
	cases := []struct {
		name     string
		handler  describeHandler
		valid    []string
		wantCode string
		wantMsg  string
	}{
		{"DescribeClusters", DescribeClusters, []string{"ATTACHMENTS", "CONFIGURATIONS", "SETTINGS", "STATISTICS", "TAGS"},
			awserrors.ErrorECSInvalidParameter, "include should be one of [ATTACHMENTS,CONFIGURATIONS,SETTINGS,STATISTICS,TAGS]"},
		{"DescribeServices", DescribeServices, []string{"TAGS"},
			awserrors.ErrorECSInvalidParameter, "include should be one of [TAGS]"},
		{"DescribeTasks", DescribeTasks, []string{"TAGS"},
			awserrors.ErrorECSInvalidParameter, "include contains an invalid value, should be one of [TAGS]"},
		{"DescribeContainerInstances", DescribeContainerInstances, []string{"TAGS", "CONTAINER_INSTANCE_HEALTH"},
			awserrors.ErrorECSInvalidParameter, "include should be one of [TAGS,CONTAINER_INSTANCE_HEALTH]"},
		{"DescribeCapacityProviders", DescribeCapacityProviders, []string{"TAGS"},
			awserrors.ErrorECSClientException, "BOGUS is not a valid value for include. The value must be one of [[TAGS]]"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/invalid", func(t *testing.T) {
			_, err := tc.handler(context.Background(), nil, "123456789012", []byte(`{"include":["TAGS","BOGUS"]}`), nil)
			code, msg, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok, "err = %v", err)
			assert.Equal(t, tc.wantCode, code)
			assert.Equal(t, tc.wantMsg, msg)
		})
		for _, v := range tc.valid {
			t.Run(tc.name+"/"+v, func(t *testing.T) {
				_, err := tc.handler(context.Background(), nil, "123456789012", []byte(`{"include":["`+v+`"]}`), nil)
				code, _, _ := awserrors.ResolveErrorDetail(err)
				assert.NotEqual(t, tc.wantCode, code, "err = %v", err)
			})
		}
	}
}
