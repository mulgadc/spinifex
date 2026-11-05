package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/zoneid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartZoneIDBackfill_PinsSeededSubnetZoneAndSurvivesOverrideChange(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	_, nc, js := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)

	d.config.Region = "mulga-lab"
	d.config.AZ = "mulga-laba"
	d.config.AZShortCode = ""
	d.natsConn = nc
	d.ctx = t.Context()

	// Seeded before any zone record exists, so only the backfill can pin it.
	vpcSvc, err := ec2vpc.NewVPCServiceImplWithNATS(t.Context(), d.config, nc)
	require.NoError(t, err)
	d.vpcService = vpcSvc
	vpcOut, err := vpcSvc.CreateVpc(t.Context(), &ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")}, testAccountID)
	require.NoError(t, err)
	_, err = vpcSvc.CreateSubnet(t.Context(), &ec2.CreateSubnetInput{
		VpcId: vpcOut.Vpc.VpcId, CidrBlock: aws.String("10.0.1.0/24"), AvailabilityZone: aws.String("rack-q"),
	}, testAccountID)
	require.NoError(t, err)

	kv, err := js.KeyValue(t.Context(), zoneid.BucketName)
	if err == nil {
		_, getErr := kv.Get(t.Context(), "zoneids.mulga-lab")
		require.Error(t, getErr, "no zone record may exist before the backfill runs")
	}

	d.startZoneIDBackfill()

	require.Eventually(t, func() bool {
		kv, err := js.KeyValue(t.Context(), zoneid.BucketName)
		if err != nil {
			return false
		}
		entry, err := kv.Get(t.Context(), "zoneids.mulga-lab")
		return err == nil && strings.Contains(string(entry.Value()), "rack-q")
	}, 10*time.Second, 50*time.Millisecond, "the backfill must pin the seeded subnet zone")

	reader, err := zoneid.NewResolver(js, "mulga-lab", "")
	require.NoError(t, err)
	pinned, err := reader.ZoneID(t.Context(), "rack-q")
	require.NoError(t, err)
	require.NotEmpty(t, pinned)

	// Restart with a different short-code override: the pinned ID must not move.
	d.config.AZShortCode = "ov99"
	d.startZoneIDBackfill()
	after, err := zoneid.NewResolver(js, "mulga-lab", "ov99")
	require.NoError(t, err)
	got, err := after.ZoneID(t.Context(), "rack-q")
	require.NoError(t, err)
	assert.Equal(t, pinned, got)
}
