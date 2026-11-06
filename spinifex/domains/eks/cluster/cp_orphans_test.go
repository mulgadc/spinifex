package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2inventory "github.com/mulgadc/spinifex/spinifex/domains/ec2/inventory"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func orphanInstance(id, eni string) *ec2.Instance {
	return &ec2.Instance{
		InstanceId:        aws.String(id),
		State:             &ec2.InstanceState{Name: aws.String(ec2.InstanceStateNameRunning)},
		LaunchTime:        aws.Time(time.Unix(0, 0)),
		Placement:         &ec2.Placement{AvailabilityZone: aws.String("ap-southeast-2a")},
		NetworkInterfaces: []*ec2.InstanceNetworkInterface{{NetworkInterfaceId: aws.String(eni)}},
	}
}

func cpTags(cluster, account string) map[string]string {
	return map[string]string{
		tags.ManagedByKey:       tags.ManagedByEKS,
		clusterEKSClusterTagKey: cluster,
		clusterEKSAccountTagKey: account,
		clusterEKSRoleTagKey:    clusterEKSRoleControlPlane,
	}
}

func workerTags(cluster, account string) map[string]string {
	m := cpTags(cluster, account)
	m[clusterEKSRoleTagKey] = "worker"
	return m
}

func TestFindControlPlaneOrphansUnreadableRecordIsNotAnOrphan(t *testing.T) {
	f := newEKSServiceFixture(t)
	js := testutil.NewJetStream(t, f.svc.deps.NATSConn)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	eni := func(context.Context, string) (map[string]string, bool, error) {
		return cpTags("gone", testAccountID), true, nil
	}
	res := []*ec2.Reservation{{Instances: []*ec2.Instance{orphanInstance("i-1", "eni-1")}}}

	rows := FindControlPlaneOrphans(ctx, js, res, eni, time.Unix(60, 0))

	require.Len(t, rows, 1)
	assert.True(t, rows[0].Unreadable, "a read that failed is not a missing record")
	assert.Error(t, rows[0].Err)
}

func TestFindControlPlaneOrphans(t *testing.T) {
	const lost = "999999999999"
	f := newEKSServiceFixture(t)
	js := testutil.NewJetStream(t, f.svc.deps.NATSConn)
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, sampleClusterMeta("alive")))

	eniTags := map[string]map[string]string{
		"eni-orphan":   cpTags("gone", testAccountID),
		"eni-live":     cpTags("alive", testAccountID),
		"eni-nobucket": cpTags("lost", lost),
		"eni-worker":   workerTags("gone", testAccountID),
	}
	eni := func(_ context.Context, id string) (map[string]string, bool, error) {
		if id == "eni-broken" {
			return nil, false, errors.New("vpc service down")
		}
		m, ok := eniTags[id]
		return m, ok, nil
	}
	res := []*ec2.Reservation{{Instances: []*ec2.Instance{
		orphanInstance("i-orphan", "eni-orphan"),
		orphanInstance("i-live", "eni-live"),
		orphanInstance("i-nobucket", "eni-nobucket"),
		orphanInstance("i-worker", "eni-worker"),
		orphanInstance("i-ghost", "eni-ghost"),
		orphanInstance("i-broken", "eni-broken"),
		{InstanceId: aws.String("i-noeni")},
	}}}

	rows := FindControlPlaneOrphans(t.Context(), js, res, eni, time.Unix(60, 0))

	got := map[string]ControlPlaneRow{}
	for _, r := range rows {
		got[r.InstanceID] = r
	}
	assert.Len(t, got, 3, "only orphan, nobucket and the unreadable ENI are reported")
	assert.Equal(t, "cluster record missing", got["i-orphan"].Reason)
	assert.False(t, got["i-orphan"].Unreadable)
	assert.Equal(t, "account bucket missing", got["i-nobucket"].Reason)
	assert.True(t, got["i-broken"].Unreadable, "a failed ENI read is never dropped")
	assert.Error(t, got["i-broken"].Err)
	assert.NotContains(t, got, "i-live", "a present record is omitted")
	assert.NotContains(t, got, "i-worker")
	assert.NotContains(t, got, "i-ghost")
	assert.NotContains(t, got, "i-noeni")
}

func TestReportControlPlaneOrphansIncompleteListingPrintsNothing(t *testing.T) {
	f := newEKSServiceFixture(t)
	js := testutil.NewJetStream(t, f.svc.deps.NATSConn)
	var buf bytes.Buffer
	list := func(context.Context) (*ec2.DescribeInstancesOutput, error) {
		return nil, errors.New("ServiceUnavailable")
	}
	orphans, unreadable, err := ReportControlPlaneOrphans(t.Context(), &buf, js, list, nil, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing incomplete")
	assert.Zero(t, orphans+unreadable)
	assert.Empty(t, buf.String(), "no partial list on a failed listing")
}

func TestReportControlPlaneOrphansPrintsRows(t *testing.T) {
	f := newEKSServiceFixture(t)
	js := testutil.NewJetStream(t, f.svc.deps.NATSConn)
	var buf bytes.Buffer
	list := func(context.Context) (*ec2.DescribeInstancesOutput, error) {
		return &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{{Instances: []*ec2.Instance{
			orphanInstance("i-orphan", "eni-orphan"), orphanInstance("i-broken", "eni-broken"),
		}}}}, nil
	}
	eni := func(_ context.Context, id string) (map[string]string, bool, error) {
		if id == "eni-broken" {
			return nil, false, errors.New("boom")
		}
		return cpTags("gone", testAccountID), true, nil
	}
	orphans, unreadable, err := ReportControlPlaneOrphans(t.Context(), &buf, js, list, eni, time.Unix(60, 0))
	require.NoError(t, err)
	assert.Equal(t, 1, orphans)
	assert.Equal(t, 1, unreadable)
	assert.Contains(t, buf.String(), "ORPHAN      i-orphan")
	assert.Contains(t, buf.String(), "UNREADABLE  i-broken")
	assert.Equal(t, 2, strings.Count(buf.String(), "az=ap-southeast-2a"), "both row kinds print the zone")
}

func TestFindControlPlaneOrphansCorruptENIRecordIsUnreadable(t *testing.T) {
	f := newEKSServiceFixture(t)
	js := testutil.NewJetStream(t, f.svc.deps.NATSConn)
	enis, err := kvutil.GetOrCreateBucket(t.Context(), js, ec2vpc.KVBucketENIs, 10)
	require.NoError(t, err)
	good, err := json.Marshal(ec2vpc.ENIRecord{NetworkInterfaceId: "eni-good", Tags: cpTags("gone", testAccountID)})
	require.NoError(t, err)
	_, err = enis.Put(t.Context(), kvutil.AccountKey("000000000000", "eni-good"), good)
	require.NoError(t, err)
	_, err = enis.Put(t.Context(), kvutil.AccountKey("000000000000", "eni-corrupt"), []byte("{not json"))
	require.NoError(t, err)
	res := []*ec2.Reservation{{Instances: []*ec2.Instance{
		orphanInstance("i-good", "eni-good"), orphanInstance("i-corrupt", "eni-corrupt"), orphanInstance("i-absent", "eni-absent"),
	}}}

	rows := FindControlPlaneOrphans(t.Context(), js, res, ec2inventory.NewENITagReader(js, "000000000000"), time.Unix(60, 0))

	require.Len(t, rows, 2, "an absent ENI is skipped, the other two are reported")
	assert.Equal(t, "i-corrupt", rows[0].InstanceID)
	assert.True(t, rows[0].Unreadable, "a corrupt ENI record is unreadable, never gone")
	assert.Equal(t, "i-good", rows[1].InstanceID)
	assert.False(t, rows[1].Unreadable)
}

func TestFindControlPlaneOrphansOnlyReportsRunningInstances(t *testing.T) {
	f := newEKSServiceFixture(t)
	js := testutil.NewJetStream(t, f.svc.deps.NATSConn)
	eni := func(context.Context, string) (map[string]string, bool, error) {
		return cpTags("gone", testAccountID), true, nil
	}
	mk := func(id, state string) *ec2.Instance {
		i := orphanInstance(id, "eni-"+id)
		i.State = &ec2.InstanceState{Name: aws.String(state)}
		return i
	}
	noState := orphanInstance("i-nostate", "eni-nostate")
	noState.State = nil
	res := []*ec2.Reservation{{Instances: []*ec2.Instance{
		mk("i-running", ec2.InstanceStateNameRunning),
		mk("i-stopped", ec2.InstanceStateNameStopped),
		mk("i-pending", ec2.InstanceStateNamePending),
		noState,
	}}}

	rows := FindControlPlaneOrphans(t.Context(), js, res, eni, time.Unix(60, 0))

	require.Len(t, rows, 1)
	assert.Equal(t, "i-running", rows[0].InstanceID)
}
