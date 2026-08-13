package handlers_elbv2

import (
	"testing"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	iammock "github.com/mulgadc/spinifex/spinifex/handlers/iam/mock"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
)

// TestEnsureLBInstanceProfile_UsesLazyProvider asserts the LB profile resolves via
// the lazy IAMProvider when the eager IAM field is unset — the daemon wires only
// the provider so the IAM build cannot race the NATS KV backend at startup.
func TestEnsureLBInstanceProfile_UsesLazyProvider(t *testing.T) {
	f := iammock.New()
	s := &ELBv2ServiceImpl{IAMProvider: func() handlers_iam.SystemInstanceRoleEnsurer { return f }}

	arn := s.ensureLBInstanceProfile(utils.GlobalAccountID)
	assert.Equal(t, "arn:aws:iam::"+utils.GlobalAccountID+":instance-profile/"+lbAgentSystemRoleName, arn)
}

// TestEnsureLBInstanceProfile_CreatesInSystemAccount asserts the role and profile
// are created under the account the LB VM runs in (the system account). IMDS
// resolves the profile under the instance's account, so a role created in the LB
// owner account would be invisible to it and the lb-agent would get no creds.
func TestEnsureLBInstanceProfile_CreatesInSystemAccount(t *testing.T) {
	f := iammock.New()
	s := &ELBv2ServiceImpl{IAM: f}

	s.ensureLBInstanceProfile(utils.GlobalAccountID)
	assert.Equal(t, utils.GlobalAccountID, f.LastRoleAcct)
	assert.Equal(t, utils.GlobalAccountID, f.LastProfileAcct)
}

// TestEnsureLBInstanceProfile_NotReadyFallsBack asserts an unwired IAM (no field,
// provider returns nil because the KV backend is not up yet) yields "" so the LB
// VM falls back to baked static creds and retries on the next launch.
func TestEnsureLBInstanceProfile_NotReadyFallsBack(t *testing.T) {
	s := &ELBv2ServiceImpl{IAMProvider: func() handlers_iam.SystemInstanceRoleEnsurer { return nil }}
	assert.Empty(t, s.ensureLBInstanceProfile(utils.GlobalAccountID))
}
