package daemon_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/network/reconcile"
	"github.com/stretchr/testify/assert"
)

// vpcd reads instance records to tell a stopped guest's port from a broken one,
// and cannot import this package to learn where they live. If these drift apart
// it silently probes every stopped instance's port again.
func TestInstanceRecordLocationMatchesVPCD(t *testing.T) {
	assert.Equal(t, daemon.InstanceStateBucket, reconcile.InstanceRecordBucket)
	assert.Equal(t, daemon.InstanceRecordPrefix, reconcile.InstanceRecordPrefix)
}
