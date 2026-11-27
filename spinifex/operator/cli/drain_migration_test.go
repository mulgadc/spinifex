package cli

import (
	"bytes"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	migrationstore "github.com/mulgadc/spinifex/spinifex/domains/migration/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrationDrainFailsBeforeConnect(t *testing.T) {
	t.Parallel()
	_, err := (&migrationDrain{}).check(t.Context())
	require.Error(t, err)
}

// A rollback below the migration level must not leave records that an older
// engine cannot reach, so every record and every empty bucket is outstanding.
func TestMigrationDrainCountsRecordsAndEmptyBuckets(t *testing.T) {
	t.Parallel()
	_, _, js := testutil.StartTestJetStream(t)
	var out bytes.Buffer
	d := &migrationDrain{}
	d.connect(js, &out)

	n, err := d.check(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.NotContains(t, out.String(), "before rolling back")

	s, err := migrationstore.OpenOrCreate(t.Context(), js, "111122223333")
	require.NoError(t, err)
	for _, id := range []string{"c-1", "c-2"} {
		_, err = s.Create(t.Context(), migrationstore.Record{Kind: "connection", ID: id, AccountID: "111122223333"})
		require.NoError(t, err)
	}
	_, err = migrationstore.OpenOrCreate(t.Context(), js, "444455556666")
	require.NoError(t, err)

	out.Reset()
	n, err = d.check(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Contains(t, out.String(), "before rolling back")
}
