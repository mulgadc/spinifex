package elbv2

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	healthLBID    = "lbhealth01"
	healthInst    = "i-lbvm01"
	healthMACData = "02:00:00:aa:00:01"
	healthMACX    = "02:00:00:aa:00:02"
)

func seedHealthLB(t *testing.T, svc *ELBv2ServiceImpl) *LoadBalancerRecord {
	t.Helper()
	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":loadbalancer/app/health-lb/" + healthLBID,
		LoadBalancerID:  healthLBID,
		Name:            "health-lb",
		State:           StateActive,
		InstanceID:      healthInst,
		AccountID:       testAccountID,
		CrossAccountENIs: []ExtraENIInput{
			{ENIID: "eni-cross01", ENIMac: healthMACX},
		},
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, svc.store.PutLoadBalancer(context.Background(), lb))
	return lb
}

func nicStatus(mac string, ok bool, failures, rx int64) *LBAgentNICStatus {
	return &LBAgentNICStatus{
		MAC:                 aws.String(mac),
		ProbeOK:             aws.Bool(ok),
		ConsecutiveFailures: aws.Int64(failures),
		RxPackets:           aws.Int64(rx),
		TxPackets:           aws.Int64(rx + 5),
		RxDropped:           aws.Int64(0),
		TxDropped:           aws.Int64(2),
	}
}

func healthBeat(t *testing.T, svc *ELBv2ServiceImpl, instanceID *string, nics ...*LBAgentNICStatus) {
	t.Helper()
	_, err := svc.LBAgentHeartbeat(context.Background(), &LBAgentHeartbeatInput{
		LBID:       aws.String(healthLBID),
		InstanceID: instanceID,
		NICs:       nics,
	}, testAccountID)
	require.NoError(t, err)
}

func healthRevision(t *testing.T, svc *ELBv2ServiceImpl) uint64 {
	t.Helper()
	entry, err := svc.store.kv.Get(context.Background(), KeyPrefixLBHealth+healthLBID)
	require.NoError(t, err)
	return entry.Revision()
}

func TestLBHealth_SteadyHeartbeatsWriteNothing(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	seedHealthLB(t, svc)

	for range 5 {
		healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, true, 0, 100))
	}
	rec, err := svc.store.GetLBHealth(context.Background(), healthLBID)
	require.NoError(t, err)
	assert.Nil(t, rec, "healthy heartbeats must not create an evidence record")

	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, false, 3, 100))
	rev := healthRevision(t, svc)
	for i := range 5 {
		healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, false, int64(4+i), int64(100+i)))
	}
	assert.Equal(t, rev, healthRevision(t, svc), "a NIC that stays failing must not rewrite the record")
}

func TestLBHealth_FailingNICWritesFailingSinceOnceAndRecoveryClears(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	seedHealthLB(t, svc)

	before := time.Now().UTC().Add(-time.Second)
	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, false, 3, 777), nicStatus(healthMACX, true, 0, 1))

	rec, err := svc.store.GetLBHealth(context.Background(), healthLBID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, healthInst, rec.InstanceID)
	require.Len(t, rec.NICs, 1)
	f := rec.NICs[healthMACData]
	require.NotNil(t, f)
	assert.True(t, f.FailingSince.After(before))
	assert.EqualValues(t, 777, f.RxPackets)
	assert.EqualValues(t, 782, f.TxPackets)
	assert.EqualValues(t, 2, f.TxDropped)
	assert.EqualValues(t, 3, f.ConsecutiveFailures)

	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, true, 0, 900))
	rec, err = svc.store.GetLBHealth(context.Background(), healthLBID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Empty(t, rec.NICs, "recovery clears the failing NIC")
}

func TestLBHealth_SecondNICFailingKeepsFirstFailingSince(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	seedHealthLB(t, svc)

	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, false, 3, 1))
	first, err := svc.store.GetLBHealth(context.Background(), healthLBID)
	require.NoError(t, err)
	since := first.NICs[healthMACData].FailingSince

	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, false, 9, 2), nicStatus(healthMACX, false, 3, 2))
	rec, err := svc.store.GetLBHealth(context.Background(), healthLBID)
	require.NoError(t, err)
	require.Len(t, rec.NICs, 2)
	assert.True(t, since.Equal(rec.NICs[healthMACData].FailingSince))
	assert.Equal(t, "eni-cross01", rec.NICs[healthMACX].ENIID, "a cross-account NIC resolves to its ENI without a describe")
}

func TestLBHealth_WrongOrMissingInstanceIDIsIgnored(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	seedHealthLB(t, svc)

	failing := nicStatus(healthMACData, false, 5, 1)
	healthBeat(t, svc, aws.String("i-someoneelse"), failing)
	healthBeat(t, svc, aws.String(""), failing)
	healthBeat(t, svc, nil, failing)

	rec, err := svc.store.GetLBHealth(context.Background(), healthLBID)
	require.NoError(t, err)
	assert.Nil(t, rec, "evidence from another VM, or with no instance ID, must be ignored")
}

func TestLBHealth_WholeRecordPutLeavesEvidenceIntact(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := seedHealthLB(t, svc)

	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, false, 3, 1))
	want, err := svc.store.GetLBHealth(context.Background(), healthLBID)
	require.NoError(t, err)
	rev := healthRevision(t, svc)

	lb.StateReason = "rewritten by an older daemon"
	require.NoError(t, svc.store.PutLoadBalancer(context.Background(), lb))

	got, err := svc.store.GetLBHealth(context.Background(), healthLBID)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, rev, healthRevision(t, svc))
}

func TestLBHealth_DeleteLoadBalancerRemovesTheKey(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := seedHealthLB(t, svc)
	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, false, 3, 1))

	_, err := svc.DeleteLoadBalancer(context.Background(), &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String(lb.LoadBalancerArn),
	}, testAccountID)
	require.NoError(t, err)

	rec, err := svc.store.GetLBHealth(context.Background(), healthLBID)
	require.NoError(t, err)
	assert.Nil(t, rec)
}

func TestLBHealth_DeleteLoadBalancerSuspendsBeforeTerminatingTheVM(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := seedHealthLB(t, svc)
	seen := make(chan bool, 1)
	svc.InstanceLauncher = &mockSystemInstanceLauncher{
		onTerminate: func(string) {
			rec, err := svc.store.GetLBHealth(context.Background(), healthLBID)
			seen <- err == nil && rec != nil && rec.Suspended
		},
	}

	_, err := svc.DeleteLoadBalancer(context.Background(), &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String(lb.LoadBalancerArn),
	}, testAccountID)
	require.NoError(t, err)

	select {
	case suspended := <-seen:
		assert.True(t, suspended, "repair must be excluded before the VM is terminated")
	case <-time.After(5 * time.Second):
		t.Fatal("the LB VM was never terminated")
	}
}

func TestLBHealth_SetSubnetsSuspendsFirstAndResumesOnTheNewVM(t *testing.T) {
	t.Parallel()
	svc, vpcSvc, mock := setupSubnetTestService(t)
	vid := vpcID(t, vpcSvc)
	sub1 := getTestSubnetID(t, vpcSvc, vid, "10.0.40.0/24", "us-east-1a")
	sub2 := getTestSubnetID(t, vpcSvc, vid, "10.0.41.0/24", "us-east-1b")

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("health-sub-lb"),
		Subnets: []*string{aws.String(sub1)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()
	lbID := aws.StringValue(out.LoadBalancers[0].LoadBalancerArn)
	rec0, err := svc.store.GetLoadBalancerByArn(context.Background(), lbID)
	require.NoError(t, err)
	require.Equal(t, "i-lb", rec0.InstanceID)

	_, err = svc.LBAgentHeartbeat(context.Background(), &LBAgentHeartbeatInput{
		LBID:       aws.String(rec0.LoadBalancerID),
		InstanceID: aws.String("i-lb"),
		NICs:       []*LBAgentNICStatus{nicStatus(healthMACData, false, 3, 1)},
	}, testAccountID)
	require.NoError(t, err)

	suspendedAtTerminate := false
	mock.onTerminate = func(string) {
		rec, gerr := svc.store.GetLBHealth(context.Background(), rec0.LoadBalancerID)
		suspendedAtTerminate = gerr == nil && rec != nil && rec.Suspended
	}
	mock.launchResult = &SystemInstanceOutput{InstanceID: "i-new", PrivateIP: "10.0.1.6"}

	_, err = svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		LoadBalancerArn: aws.String(lbID),
		Subnets:         []*string{aws.String(sub1), aws.String(sub2)},
	}, testAccountID)
	require.NoError(t, err)
	assert.True(t, suspendedAtTerminate, "repair must be excluded before the VM is terminated")

	rec, err := svc.store.GetLBHealth(context.Background(), rec0.LoadBalancerID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.False(t, rec.Suspended, "the suspension is lifted once the new VM is published")
	assert.Equal(t, "i-new", rec.InstanceID)
	assert.Empty(t, rec.NICs, "the old VM's failing NIC does not carry over")
}

func TestLBHealth_SetSubnetsFailureBeforeTheVMIsTouchedResumes(t *testing.T) {
	t.Parallel()
	svc, vpcSvc, mock := setupSubnetTestService(t)
	vid := vpcID(t, vpcSvc)
	sub1 := getTestSubnetID(t, vpcSvc, vid, "10.0.42.0/24", "us-east-1a")
	sub2 := getTestSubnetID(t, vpcSvc, vid, "10.0.43.0/24", "us-east-1b")

	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("health-resume-lb"),
		Subnets: []*string{aws.String(sub1)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()
	arn := aws.StringValue(out.LoadBalancers[0].LoadBalancerArn)
	rec0, err := svc.store.GetLoadBalancerByArn(context.Background(), arn)
	require.NoError(t, err)

	mock.terminateErr = assert.AnError
	_, err = svc.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
		LoadBalancerArn: aws.String(arn),
		Subnets:         []*string{aws.String(sub1), aws.String(sub2)},
	}, testAccountID)
	require.Error(t, err)

	rec, err := svc.store.GetLBHealth(context.Background(), rec0.LoadBalancerID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.False(t, rec.Suspended, "a failed reshape that left the VM running must not leave repair excluded")
}

func TestLBHealth_DescribeShowsImpairedOnlyForFailingNICOfCurrentVM(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := seedHealthLB(t, svc)
	code := func() string {
		out, err := svc.DescribeLoadBalancers(context.Background(), &elbv2.DescribeLoadBalancersInput{
			LoadBalancerArns: []*string{aws.String(lb.LoadBalancerArn)},
		}, testAccountID)
		require.NoError(t, err)
		require.Len(t, out.LoadBalancers, 1)
		return aws.StringValue(out.LoadBalancers[0].State.Code)
	}

	assert.Equal(t, StateActive, code())

	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, false, 3, 1))
	assert.Equal(t, StateActiveImpaired, code())

	healthBeat(t, svc, aws.String(healthInst), nicStatus(healthMACData, true, 0, 2))
	assert.Equal(t, StateActive, code(), "active again once the probe recovers")

	// Evidence recorded against a VM the LB no longer names never impairs it.
	_, err := svc.store.UpdateLBHealth(context.Background(), healthLBID, func(r *LBDataPathRecord) (bool, error) {
		r.InstanceID = "i-oldvm"
		r.NICs = map[string]*LBNICFailure{healthMACData: {FailingSince: time.Now().UTC()}}
		return true, nil
	})
	require.NoError(t, err)
	assert.Equal(t, StateActive, code())

	// A LB that is not active is reported as it is.
	_, err = svc.store.UpdateLBHealth(context.Background(), healthLBID, func(r *LBDataPathRecord) (bool, error) {
		r.InstanceID = healthInst
		return true, nil
	})
	require.NoError(t, err)
	require.Equal(t, StateActiveImpaired, code())
	_, err = svc.store.UpdateLoadBalancer(context.Background(), healthLBID, func(r *LoadBalancerRecord) (bool, error) {
		r.State = StateFailed
		return true, nil
	})
	require.NoError(t, err)
	assert.Equal(t, StateFailed, code())
}

// failKeysKV fails key listing once armed, which is what the listener cascade
// of DeleteLoadBalancer reads first.
type failKeysKV struct {
	jetstream.KeyValue

	armed atomic.Bool
}

func (k *failKeysKV) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	if k.armed.Load() {
		return nil, errors.New("injected key listing failure")
	}
	return k.KeyValue.Keys(ctx, opts...)
}

func TestLBHealth_DeleteLoadBalancerListenerCascadeFailureResumes(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lb := seedHealthLB(t, svc)
	kv := &failKeysKV{KeyValue: svc.store.kv}
	svc.store.kv = kv
	mock := &mockSystemInstanceLauncher{}
	svc.InstanceLauncher = mock
	kv.armed.Store(true)

	_, err := svc.DeleteLoadBalancer(context.Background(), &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String(lb.LoadBalancerArn),
	}, testAccountID)
	require.Error(t, err)
	kv.armed.Store(false)

	rec, err := svc.store.GetLBHealth(context.Background(), healthLBID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.False(t, rec.Suspended, "a delete that failed before touching the VM must not leave repair excluded")
	assert.Empty(t, mock.terminateCalls)
}
