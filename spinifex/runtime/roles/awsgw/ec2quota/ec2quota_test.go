package ec2quota_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/ec2quota"
	"github.com/stretchr/testify/assert"
)

func TestAdapter_ChargeLaunchPassesThrough(t *testing.T) {
	a := ec2quota.Adapter{Service: admissionquota.New(admissionquota.Limits{}, nil)}
	assert.NoError(t, a.ChargeLaunch(context.Background(), "123456789012", &ec2.Reservation{}))
}

func TestAdapter_EnforceRetypeWithNoLimitsChargesNothing(t *testing.T) {
	a := ec2quota.Adapter{Service: admissionquota.New(admissionquota.Limits{}, nil)}
	delta, err := a.EnforceRetype(context.Background(), nil, func() []string { return nil }, "123456789012", "i-missing", "t3.micro")
	assert.NoError(t, err)
	assert.Zero(t, delta)
}
