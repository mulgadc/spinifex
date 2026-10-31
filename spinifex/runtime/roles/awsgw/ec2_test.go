package awsgw

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"

	admissionquota "github.com/mulgadc/spinifex/spinifex/domains/admission/quota"
	ec2awsapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi"
	"github.com/stretchr/testify/assert"
)

func TestAdvertisedEndpoint(t *testing.T) {
	assert.Equal(t, "https://localhost:9999", advertisedEndpoint("", ""))
	assert.Equal(t, "https://gw.example:8443", advertisedEndpoint("gw.example", "8443"))
	assert.Equal(t, "https://gw.example", advertisedEndpoint("gw.example", "443"))
}

func TestNewEC2Registration(t *testing.T) {
	reg := newEC2Registration(ec2Wiring{
		Quota:         admissionquota.New(admissionquota.Limits{}, nil),
		ExpectedNodes: 2,
		RegistryHost:  "gw.example",
		RegistryPort:  "443",
	})

	assert.Equal(t, ec2awsapi.ServiceName, reg.Service)
	assert.NotNil(t, reg.Dispatch)
	assert.ElementsMatch(t, ec2awsapi.ActionNames(), reg.Inventory.Registered)
}

func TestStartInstanceStatus(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	status := startInstanceStatus(ctx, js)

	assert.NotNil(t, status.Records)
	assert.NotNil(t, status.Liveness)
}
