package accountteardown

//test:in-package — the reapers are unexported, and the snapshot choice and
// default-group filter asserted here are the substance of them.

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRDS answers the rds.* subjects, with per-subject reply queues so a
// delete that succeeds only after a modify can be scripted.
type fakeRDS struct {
	nc *nats.Conn

	mu        sync.Mutex
	calls     []string
	requests  map[string][][]byte
	queued    map[string][]any
	errorCode map[string][]string
}

func newFakeRDS(t *testing.T) *fakeRDS {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)

	fake := &fakeRDS{
		nc:        nc,
		requests:  map[string][][]byte{},
		queued:    map[string][]any{},
		errorCode: map[string][]string{},
	}

	sub, err := nc.Subscribe("rds.>", fake.serve)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	return fake
}

func (f *fakeRDS) serve(msg *nats.Msg) {
	f.mu.Lock()
	f.calls = append(f.calls, msg.Subject)
	f.requests[msg.Subject] = append(f.requests[msg.Subject], msg.Data)

	code := ""
	if queue := f.errorCode[msg.Subject]; len(queue) > 0 {
		code = queue[0]
		f.errorCode[msg.Subject] = queue[1:]
	}

	var reply any
	hasReply := false
	if queue := f.queued[msg.Subject]; len(queue) > 0 {
		reply = queue[0]
		hasReply = true
		if len(queue) > 1 {
			f.queued[msg.Subject] = queue[1:]
		}
	}
	f.mu.Unlock()

	switch {
	case code != "":
		_ = msg.Respond(utils.GenerateErrorPayload(code))
	case hasReply:
		payload, err := json.Marshal(reply)
		if err != nil {
			_ = msg.Respond(utils.GenerateErrorPayload("InternalError"))
			return
		}
		_ = msg.Respond(payload)
	default:
		_ = msg.Respond([]byte(`{}`))
	}
}

func (f *fakeRDS) reply(subject string, outputs ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued[subject] = append(f.queued[subject], outputs...)
}

// failNext queues one error per call, in order, so a first attempt can fail
// and a retry succeed.
func (f *fakeRDS) failNext(subject string, codes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errorCode[subject] = append(f.errorCode[subject], codes...)
}

func (f *fakeRDS) requestAt(t *testing.T, subject string, index int, into any) {
	t.Helper()
	f.mu.Lock()
	payloads := f.requests[subject]
	f.mu.Unlock()
	require.Greater(t, len(payloads), index, "subject %s was called %d times", subject, len(payloads))
	require.NoError(t, json.Unmarshal(payloads[index], into))
}

func (f *fakeRDS) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newRDSReapers(nc *nats.Conn) (*rdsInstanceReaper, *rdsSubnetGroupReaper, *rdsParameterGroupReaper) {
	svc := handlers_rds.NewNATSService(nc)
	return &rdsInstanceReaper{svc: svc}, &rdsSubnetGroupReaper{svc: svc}, &rdsParameterGroupReaper{svc: svc}
}

// A deleting instance still holds its VM, volume and ENI, so it stays listed
// until it is actually gone rather than counting as drained when asked to go.
func TestRDSInstanceReaperListsInstancesThatAreAlreadyDeleting(t *testing.T) {
	fake := newFakeRDS(t)
	fake.reply("rds.DescribeDBInstances", rds.DescribeDBInstancesOutput{
		DBInstances: []*rds.DBInstance{
			{DBInstanceIdentifier: aws.String("orders"), DBInstanceStatus: aws.String("available")},
			{DBInstanceIdentifier: aws.String("reports"), DBInstanceStatus: aws.String("deleting")},
		},
	})

	instances, _, _ := newRDSReapers(fake.nc)
	found, err := instances.List(testCtx(t), "000000000042")
	require.NoError(t, err)

	assert.Equal(t, []Resource{
		{Kind: "rds-instance", ID: "orders", Detail: "available"},
		{Kind: "rds-instance", ID: "reports", Detail: "deleting"},
	}, found)
}

// The AWS-faithful default takes a final snapshot. Inside an account being
// deleted that either forces the storage stage to remove it again or leaks a
// snapshot with no owner, so teardown must always skip it.
func TestRDSInstanceReaperAlwaysSkipsTheFinalSnapshot(t *testing.T) {
	fake := newFakeRDS(t)
	fake.reply("rds.DeleteDBInstance", rds.DeleteDBInstanceOutput{})

	instances, _, _ := newRDSReapers(fake.nc)
	require.NoError(t, instances.Delete(testCtx(t), "000000000042",
		Resource{Kind: "rds-instance", ID: "orders"}, false))

	var input rds.DeleteDBInstanceInput
	fake.requestAt(t, "rds.DeleteDBInstance", 0, &input)
	assert.True(t, aws.BoolValue(input.SkipFinalSnapshot))
	assert.Empty(t, aws.StringValue(input.FinalDBSnapshotIdentifier))
}

// Deletion protection is RDS's version of the deadlock --force exists for: the
// tenant is gone and nobody is left to clear the flag through the ordinary API.
func TestRDSInstanceReaperForceClearsDeletionProtection(t *testing.T) {
	fake := newFakeRDS(t)
	fake.failNext("rds.DeleteDBInstance",
		"InvalidParameterCombination: DB instance orders cannot be deleted because deletion protection is enabled")
	fake.reply("rds.ModifyDBInstance", rds.ModifyDBInstanceOutput{})
	fake.reply("rds.DeleteDBInstance", rds.DeleteDBInstanceOutput{})

	instances, _, _ := newRDSReapers(fake.nc)
	require.NoError(t, instances.Delete(testCtx(t), "000000000042",
		Resource{Kind: "rds-instance", ID: "orders"}, true))

	var modify rds.ModifyDBInstanceInput
	fake.requestAt(t, "rds.ModifyDBInstance", 0, &modify)
	assert.False(t, aws.BoolValue(modify.DeletionProtection))
	assert.True(t, aws.BoolValue(modify.ApplyImmediately))

	assert.Equal(t, []string{
		"rds.DeleteDBInstance", "rds.ModifyDBInstance", "rds.DeleteDBInstance",
	}, fake.called(), "the flag is cleared only after the ordinary delete refuses")
}

// Without --force the refusal stands. Silently disarming a protection the
// operator did not ask to override is not teardown's call to make.
func TestRDSInstanceReaperLeavesDeletionProtectionAloneWithoutForce(t *testing.T) {
	fake := newFakeRDS(t)
	fake.failNext("rds.DeleteDBInstance",
		"InvalidParameterCombination: DB instance orders cannot be deleted because deletion protection is enabled")

	instances, _, _ := newRDSReapers(fake.nc)
	err := instances.Delete(testCtx(t), "000000000042",
		Resource{Kind: "rds-instance", ID: "orders"}, false)

	require.Error(t, err)
	assert.NotContains(t, fake.called(), "rds.ModifyDBInstance")
}

// Force is not a licence to escalate past every refusal — only past the one it
// can do something about. A busy instance is retried by the drain loop instead.
func TestRDSInstanceReaperForceDoesNotEscalatePastOtherFailures(t *testing.T) {
	fake := newFakeRDS(t)
	fake.failNext("rds.DeleteDBInstance", "InternalError")

	instances, _, _ := newRDSReapers(fake.nc)
	err := instances.Delete(testCtx(t), "000000000042",
		Resource{Kind: "rds-instance", ID: "orders"}, true)

	require.Error(t, err)
	assert.NotContains(t, fake.called(), "rds.ModifyDBInstance")
}

// RDS synthesises a default parameter group per engine into every listing,
// whether or not anything created one, and refuses to delete it. Offering them
// up would leave the platform stage unable to drain for any account at all.
func TestRDSParameterGroupReaperSkipsTheSynthesisedDefaults(t *testing.T) {
	fake := newFakeRDS(t)
	fake.reply("rds.DescribeDBParameterGroups", rds.DescribeDBParameterGroupsOutput{
		DBParameterGroups: []*rds.DBParameterGroup{
			{DBParameterGroupName: aws.String("default.postgres16")},
			{DBParameterGroupName: aws.String("Default.MariaDB11")},
			{DBParameterGroupName: aws.String("app-tuned")},
		},
	})

	_, _, groups := newRDSReapers(fake.nc)
	found, err := groups.List(testCtx(t), "000000000042")
	require.NoError(t, err)

	assert.Equal(t, []Resource{{Kind: "rds-parameter-group", ID: "app-tuned"}}, found)
}

func TestRDSSubnetGroupReaperListsAndDeletes(t *testing.T) {
	fake := newFakeRDS(t)
	fake.reply("rds.DescribeDBSubnetGroups", rds.DescribeDBSubnetGroupsOutput{
		DBSubnetGroups: []*rds.DBSubnetGroup{{DBSubnetGroupName: aws.String("db-private")}},
	})
	fake.reply("rds.DeleteDBSubnetGroup", rds.DeleteDBSubnetGroupOutput{})

	_, subnets, _ := newRDSReapers(fake.nc)
	found, err := subnets.List(testCtx(t), "000000000042")
	require.NoError(t, err)
	require.Equal(t, []Resource{{Kind: "rds-subnet-group", ID: "db-private"}}, found)

	require.NoError(t, subnets.Delete(testCtx(t), "000000000042", found[0], false))

	var input rds.DeleteDBSubnetGroupInput
	fake.requestAt(t, "rds.DeleteDBSubnetGroup", 0, &input)
	assert.Equal(t, "db-private", aws.StringValue(input.DBSubnetGroupName))
}

func TestRDSReaperTreatsAMissingResourceAsDeleted(t *testing.T) {
	fake := newFakeRDS(t)
	fake.failNext("rds.DeleteDBInstance", "DBInstanceNotFound")
	fake.failNext("rds.DeleteDBSubnetGroup", "DBSubnetGroupNotFoundFault")
	fake.failNext("rds.DeleteDBParameterGroup", "DBParameterGroupNotFound")

	instances, subnets, groups := newRDSReapers(fake.nc)
	assert.NoError(t, instances.Delete(testCtx(t), "000000000042", Resource{ID: "gone"}, false))
	assert.NoError(t, subnets.Delete(testCtx(t), "000000000042", Resource{ID: "gone"}, false))
	assert.NoError(t, groups.Delete(testCtx(t), "000000000042", Resource{ID: "gone"}, false))
}

// A configuration group cannot be removed while an instance still references
// it, so the groups wait for a later stage than the instances do.
func TestRDSGroupsAreReapedAfterInstances(t *testing.T) {
	reapers := RDSReapers(nil)

	assert.Equal(t, StageCompute, reapers[indexOfKind(reapers, "rds-instance")].Stage())
	assert.Equal(t, StagePlatform, reapers[indexOfKind(reapers, "rds-subnet-group")].Stage())
	assert.Equal(t, StagePlatform, reapers[indexOfKind(reapers, "rds-parameter-group")].Stage())
}
