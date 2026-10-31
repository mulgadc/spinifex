package daemon

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/internal/testkit"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A system VM on a customer ENI is only resolvable by IMDS once the ENI record
// names the system account as the instance owner. Without it the VM never gets
// role credentials and its load balancer is reaped as failed.
func TestLaunchSystemInstance_OwnerStampSurvivesStaleReplicaRead(t *testing.T) {
	f := newSysInstFixture(t)
	f.d.vpcService.WrapENIKVForTest(func(kv jetstream.KeyValue) jetstream.KeyValue { return testutil.NewStaleReadKV(kv) })
	sysInstLaunchRuns(f.d)

	eni := f.newENI(t)
	out, err := f.d.LaunchSystemInstance(f.input(eni))
	require.NoError(t, err)

	rec := f.eniRecord(t, testAccountID, aws.StringValue(eni.NetworkInterfaceId))
	assert.Equal(t, out.InstanceID, rec.InstanceId)
	assert.Equal(t, awsidentifiers.GlobalAccountID, rec.InstanceOwnerId,
		"the owner stamp must land even when the read behind it was served by a lagging replica")
}

func TestLaunchSystemInstance_UnwritableOwnerStampFailsTheLaunch(t *testing.T) {
	f := newSysInstFixture(t)
	f.d.vpcService.WrapENIKVForTest(func(kv jetstream.KeyValue) jetstream.KeyValue {
		return testutil.NewRejectingKV(kv, func(_ string, value []byte) error {
			var rec ec2vpc.ENIRecord
			if json.Unmarshal(value, &rec) == nil && rec.InstanceOwnerId != "" {
				return errors.New("stream unavailable")
			}
			return nil
		})
	})
	sysInstLaunchRuns(f.d)

	eni := f.newENI(t)
	_, err := f.d.LaunchSystemInstance(f.input(eni))
	require.Error(t, err, "a VM whose ENI cannot name its owner can never authenticate and must not be launched")

	require.Eventually(t, func() bool { return len(f.d.vmMgr.Snapshot()) == 0 }, 5*time.Second, 20*time.Millisecond,
		"the failed launch must leave no VM behind")
}
