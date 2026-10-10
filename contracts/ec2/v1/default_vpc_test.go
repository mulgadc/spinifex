package ec2v1_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
)

// The gateway's CreateAccount request and the daemon's subscription meet on
// this literal; a deployed daemon and gateway of different builds must agree.
func TestEnsureDefaultVpcSubject(t *testing.T) {
	require.Equal(t, "ec2.EnsureDefaultVpc", ec2v1.EnsureDefaultVpcSubject)
}
