package elbv2

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fwdALBArn = "arn:aws:elasticloadbalancing:us-east-1:1:loadbalancer/x/alb-fwd/alb-fwd"

func putFwdALB(t *testing.T, svc *ELBv2ServiceImpl) {
	t.Helper()
	putModifyFixture(t, svc, ProtocolHTTP)
	putModifyLB(t, svc, "alb-fwd", LoadBalancerTypeApplication, ProtocolHTTP)
}

func modifyLBAttr(t *testing.T, svc *ELBv2ServiceImpl, kv ...string) error {
	t.Helper()
	var attrs []*elbv2.LoadBalancerAttribute
	for i := 0; i < len(kv); i += 2 {
		attrs = append(attrs, &elbv2.LoadBalancerAttribute{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
	}
	_, err := svc.ModifyLoadBalancerAttributes(t.Context(), &elbv2.ModifyLoadBalancerAttributesInput{
		LoadBalancerArn: aws.String(fwdALBArn),
		Attributes:      attrs,
	}, testAccountID)
	return err
}

func TestModifyLoadBalancerAttributes_AcceptsForwardingValues(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"append", "preserve", "remove"} {
		for _, port := range []string{"true", "false"} {
			t.Run(mode+"/"+port, func(t *testing.T) {
				t.Parallel()
				svc := setupTestService(t)
				putFwdALB(t, svc)
				require.NoError(t, modifyLBAttr(t, svc, AttrXFFHeaderProcessing, mode, AttrXFFClientPort, port))
				got := getLB(t, svc, "alb-fwd")
				assert.Equal(t, mode, got.Attributes[AttrXFFHeaderProcessing])
				assert.Equal(t, port, got.Attributes[AttrXFFClientPort])
			})
		}
	}
}

func TestModifyLoadBalancerAttributes_RejectsBadForwardingValues(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, key, value string }{
		{"mode unknown", AttrXFFHeaderProcessing, "prepend"},
		{"mode wrong case", AttrXFFHeaderProcessing, "Append"},
		{"mode empty", AttrXFFHeaderProcessing, ""},
		{"client port word", AttrXFFClientPort, "yes"},
		{"client port wrong case", AttrXFFClientPort, "True"},
		{"client port empty", AttrXFFClientPort, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc := setupTestService(t)
			putFwdALB(t, svc)
			before := getLB(t, svc, "alb-fwd")

			// A valid pair submitted alongside must not be stored either.
			err := modifyLBAttr(t, svc, "idle_timeout.timeout_seconds", "120", tt.key, tt.value)

			require.Error(t, err)
			code, ok := awserrors.ResolveErrorCode(err)
			require.True(t, ok)
			assert.Equal(t, awserrors.ErrorValidationError, code)
			_, msg, _ := awserrors.ResolveErrorDetail(err)
			assert.Contains(t, msg, tt.key)
			assert.Equal(t, before, getLB(t, svc, "alb-fwd"), "a rejected call leaves the record untouched")
		})
	}
}

func TestModifyLoadBalancerAttributes_ForwardingChangeReRendersConfig(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	putFwdALB(t, svc)
	before := getLB(t, svc, "alb-fwd")
	require.Contains(t, before.ConfigText, `set-header X-Forwarded-For "%[src]"`)

	require.NoError(t, modifyLBAttr(t, svc, AttrXFFClientPort, "true"))
	withPort := getLB(t, svc, "alb-fwd")
	assert.Contains(t, withPort.ConfigText, "src_port")
	assert.NotEqual(t, before.ConfigHash, withPort.ConfigHash)

	require.NoError(t, modifyLBAttr(t, svc, AttrXFFHeaderProcessing, "remove"))
	removed := getLB(t, svc, "alb-fwd")
	assert.Contains(t, removed.ConfigText, "del-header X-Forwarded-For")
	assert.NotContains(t, removed.ConfigText, "set-header X-Forwarded-For")
	assert.NotEqual(t, withPort.ConfigHash, removed.ConfigHash)
}

func TestModifyLoadBalancerAttributes_UnchangedOrUnrelatedKeysLeaveTheConfigAlone(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	putFwdALB(t, svc)
	require.NoError(t, modifyLBAttr(t, svc, AttrXFFHeaderProcessing, "preserve"))
	before := getLB(t, svc, "alb-fwd")

	require.NoError(t, modifyLBAttr(t, svc, AttrXFFHeaderProcessing, "preserve"))
	assert.Equal(t, before, getLB(t, svc, "alb-fwd"), "same value: nothing written")

	require.NoError(t, modifyLBAttr(t, svc, "idle_timeout.timeout_seconds", "120"))
	after := getLB(t, svc, "alb-fwd")
	assert.Equal(t, before.ConfigHash, after.ConfigHash, "an attribute the config does not render keeps the hash")
	assert.Equal(t, "120", after.Attributes["idle_timeout.timeout_seconds"])
}
