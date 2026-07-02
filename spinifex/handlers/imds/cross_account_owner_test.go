package handlers_imds

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A same-account ENI leaves InstanceOwnerId empty, so iamAccountID falls back to
// the ENI's own account — the common EC2 case where instance and ENI co-own.
func TestIAMAccountID_FallsBackToENIAccount(t *testing.T) {
	eni := &eniFacts{accountID: "111122223333"}
	assert.Equal(t, "111122223333", eni.iamAccountID())
}

// A system VM (LB/EKS) plugs into a customer ENI: the ENI's own account differs
// from the account that owns the instance and its IAM role. iamAccountID returns
// the instance-owner account so the role lookup hits the right tenant.
func TestIAMAccountID_PrefersInstanceOwner(t *testing.T) {
	eni := &eniFacts{accountID: "000000000001", instanceAccountID: "000000000000"}
	assert.Equal(t, "000000000000", eni.iamAccountID())
}

// eniFactsFromRecord projects the persisted InstanceOwnerId onto the fact the
// IAM/STS resolution keys off, so a cross-account attach survives the round-trip.
func TestEniFactsFromRecord_CarriesInstanceOwner(t *testing.T) {
	rec := &eniRecord{
		NetworkInterfaceId: "eni-lb",
		InstanceId:         "i-sysvm",
		InstanceOwnerId:    "000000000000",
	}
	facts := eniFactsFromRecord("000000000001", rec)
	assert.Equal(t, "000000000001", facts.accountID)
	assert.Equal(t, "000000000000", facts.instanceAccountID)
	assert.Equal(t, "000000000000", facts.iamAccountID())
}

// resolveInstance must describe the instance under its owning account, not the
// ENI account — a system VM is invisible under the customer-account fan-out, so
// keying the describe off the ENI account would resolve no instance at all.
func TestResolveInstance_DescribesUnderInstanceOwner(t *testing.T) {
	r, lookup := newTestResolver(t)
	lookup.facts = &instanceFacts{iamInstanceProfileArn: "arn:aws:iam::000000000000:instance-profile/spinifex-lb-agent"}

	eni := &eniFacts{accountID: "000000000001", instanceAccountID: "000000000000", instanceID: "i-sysvm"}
	inst, err := r.resolveInstance(eni)
	require.NoError(t, err)
	require.NotNil(t, inst)
	assert.Equal(t, "000000000000", lookup.lastAccount)
}

// Without an instance-owner stamp the describe routes to the ENI account, the
// historical same-account behaviour the fallback preserves.
func TestResolveInstance_FallsBackToENIAccount(t *testing.T) {
	r, lookup := newTestResolver(t)
	lookup.facts = &instanceFacts{}

	eni := &eniFacts{accountID: "111122223333", instanceID: "i-app"}
	_, err := r.resolveInstance(eni)
	require.NoError(t, err)
	assert.Equal(t, "111122223333", lookup.lastAccount)
}
