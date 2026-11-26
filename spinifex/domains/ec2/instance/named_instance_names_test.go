package instance_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon"
	"github.com/stretchr/testify/assert"
)

func TestNamedInstanceReader_RecordSpaceMatchesDaemon(t *testing.T) {
	assert.Equal(t, daemon.InstanceStateBucket, instance.InstanceStateBucketName)
	assert.Equal(t, daemon.InstanceRecordPrefix, instance.InstanceRecordPrefixName)
}
