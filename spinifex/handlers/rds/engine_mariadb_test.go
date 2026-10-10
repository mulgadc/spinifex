package handlers_rds

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	rdsengine "github.com/mulgadc/spinifex/spinifex/domains/rds/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole point of registering the engine: a request naming it has to reach
// the MariaDB AMI carrying MariaDB's own port, family and guest configuration,
// not PostgreSQL's defaults with a different name on them.
func TestCreateDBInstance_LaunchesMariaDB(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, testBaseDomain)
	input := validCreateInput()
	input.Engine = aws.String("mariadb")

	out, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, out.DBInstance)
	assert.Equal(t, "mariadb", aws.StringValue(out.DBInstance.Engine))
	assert.Equal(t, "11.8", aws.StringValue(out.DBInstance.EngineVersion))
	require.NotNil(t, out.DBInstance.Endpoint)
	assert.Equal(t, int64(3306), aws.Int64Value(out.DBInstance.Endpoint.Port),
		"the engine's default port applies when the request names none")

	rec := h.record(t, testDBInstanceID)
	assert.Equal(t, "mariadb", rec.Engine)
	assert.Equal(t, int64(3306), rec.Port)
	mariadb, err := rdsengine.LookupEngine("mariadb")
	require.NoError(t, err)
	assert.Equal(t, mariadb.DefaultParameterGroupName(), rec.DBParameterGroupName)

	// The AMI is selected by the engine's own tags, so a MariaDB create can never
	// land on the PostgreSQL image however the two are published.
	amiFilters := map[string]string{}
	for _, f := range h.launch.images.filters {
		amiFilters[aws.StringValue(f.Name)] = aws.StringValue(f.Values[0])
	}
	assert.Equal(t, "mariadb", amiFilters["tag:"+engineTagKey])
	assert.Equal(t, "11.8", amiFilters["tag:"+engineVersionTagKey])

	// The guest builds its engine implementation from the image, and refuses to
	// run when cloud-init disagrees with what the image bakes.
	require.NotNil(t, h.launch.launcher.input)
	assert.Contains(t, h.launch.launcher.input.UserData, "RDS_ENGINE=mariadb")
	assert.Contains(t, h.launch.launcher.input.UserData, "RDS_ENGINE_PORT=3306")
}
