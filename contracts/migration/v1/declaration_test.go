package migrationv1

import (
	"testing"

	accountlifecyclev1 "github.com/mulgadc/spinifex/contracts/accountlifecycle/v1"
	"github.com/stretchr/testify/require"
)

func TestDeclarationIsWireStable(t *testing.T) {
	d := Declaration()
	require.Equal(t, "migration", d.Name)
	require.True(t, accountlifecyclev1.ValidParticipantName(d.Name))
	require.Equal(t, []string{"connection", "assessment", "copy-task"}, d.Classes)
	require.Equal(t, 1, d.DeclarationVersion)
	require.Positive(t, d.DrainBudget)

	d.Classes[0] = "changed"
	require.Equal(t, ClassConnection, Declaration().Classes[0], "a caller cannot change the declaration")
}
