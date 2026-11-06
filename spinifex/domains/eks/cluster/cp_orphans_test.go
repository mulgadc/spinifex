package cluster

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func orphanInstance(id, eni string) *ec2.Instance {
	return &ec2.Instance{
		InstanceId:        aws.String(id),
		LaunchTime:        aws.Time(time.Unix(0, 0)),
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
}
