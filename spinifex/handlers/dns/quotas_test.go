package dns

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultQuotasMirrorAWS(t *testing.T) {
	q := DefaultQuotas()
	assert.Equal(t, 500, q.HostedZonesPerAccount)
	assert.Equal(t, 10_000, q.RecordsPerHostedZone)
	assert.Equal(t, 300, q.VPCsPerPrivateZone)
	assert.Equal(t, 10_000, q.ResolverQPSPerIP)
}

func TestWithinRecordQuotaBoundary(t *testing.T) {
	q := Quotas{RecordsPerHostedZone: 3}
	assert.True(t, q.withinRecordQuota(2), "below the cap may add")
	assert.False(t, q.withinRecordQuota(3), "at the cap may not add")
	assert.False(t, q.withinRecordQuota(4), "over the cap may not add")
}
