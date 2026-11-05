package accountlifecycle

//test:in-package — the ECS reaper is unexported for the same reason the EC2
// ones are, and what matters here is its listing shape and the position it is
// registered in.

import (
	"context"
	"encoding/json"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/internal/testkit"
	ecsdomain "github.com/mulgadc/spinifex/spinifex/domains/ecs"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeECS answers the ecs.* subjects the reaper drives.
type fakeECS struct {
	nc *nats.Conn

	mu        sync.Mutex
	calls     []string
	requests  map[string][]byte
	replies   map[string]any
	errorCode map[string]string
}

func newFakeECS(t *testing.T) *fakeECS {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)

	cluster := &fakeECS{
		nc:        nc,
		requests:  map[string][]byte{},
		replies:   map[string]any{},
		errorCode: map[string]string{},
	}

	sub, err := nc.Subscribe("ecs.>", cluster.serve)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	return cluster
}

func (c *fakeECS) serve(msg *nats.Msg) {
	c.mu.Lock()
	c.calls = append(c.calls, msg.Subject)
	c.requests[msg.Subject] = msg.Data
	code, isError := c.errorCode[msg.Subject]
	reply, hasReply := c.replies[msg.Subject]
	c.mu.Unlock()

	switch {
	case isError:
		_ = msg.Respond(awserrors.GenerateErrorPayload(code))
	case hasReply:
		payload, err := json.Marshal(reply)
		if err != nil {
			_ = msg.Respond(awserrors.GenerateErrorPayload("InternalError"))
			return
		}
		_ = msg.Respond(payload)
	default:
		_ = msg.Respond([]byte(`{}`))
	}
}

func (c *fakeECS) reply(subject string, output any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replies[subject] = output
}

func (c *fakeECS) fail(subject, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errorCode[subject] = code
}

func (c *fakeECS) request(t *testing.T, subject string, into any) {
	t.Helper()
	c.mu.Lock()
	payload, ok := c.requests[subject]
	c.mu.Unlock()
	require.True(t, ok, "subject %s was never called", subject)
	require.NoError(t, json.Unmarshal(payload, into))
}

func newECSReaper(nc *nats.Conn) *ecsClusterReaper {
	return &ecsClusterReaper{svc: ecsdomain.NewNATSECSService(nc)}
}

// The listing is keyed by cluster name, which is the identity ECS takes back
// and the only part of the ARN an operator reading a stuck line can act on.
func TestECSClusterReaperListsByName(t *testing.T) {
	cluster := newFakeECS(t)
	cluster.reply("ecs.ListClusters", ecs.ListClustersOutput{
		ClusterArns: []*string{
			aws.String("arn:aws:ecs:us-west-1:000000000042:cluster/web"),
			aws.String("arn:aws:ecs:us-west-1:000000000042:cluster/batch"),
		},
	})

	found, err := newECSReaper(cluster.nc).List(testCtx(t), "000000000042")
	require.NoError(t, err)

	assert.Equal(t, []Resource{
		{Kind: "ecs-cluster", ID: "web"},
		{Kind: "ecs-cluster", ID: "batch"},
	}, found)
}

// An account with no ECS at all must list empty rather than error, or the
// compute stage would never drain for the overwhelming majority of tenants.
func TestECSClusterReaperListsNothingForAnAccountWithNoClusters(t *testing.T) {
	cluster := newFakeECS(t)
	cluster.reply("ecs.ListClusters", ecs.ListClustersOutput{})

	found, err := newECSReaper(cluster.nc).List(testCtx(t), "000000000042")
	require.NoError(t, err)

	assert.Empty(t, found)
}

func TestECSClusterReaperDeletesByName(t *testing.T) {
	cluster := newFakeECS(t)
	cluster.reply("ecs.DeleteCluster", ecs.DeleteClusterOutput{})

	err := newECSReaper(cluster.nc).Delete(testCtx(t), "000000000042",
		Resource{Kind: "ecs-cluster", ID: "web"}, false)
	require.NoError(t, err)

	var input ecs.DeleteClusterInput
	cluster.request(t, "ecs.DeleteCluster", &input)
	assert.Equal(t, "web", aws.StringValue(input.Cluster))
}

// Teardown re-runs after a crash, so a cluster a previous pass already removed
// is a success. Treating it as a failure would leave the account undeletable.
func TestECSClusterReaperTreatsAMissingClusterAsDeleted(t *testing.T) {
	cluster := newFakeECS(t)
	cluster.fail("ecs.DeleteCluster", "ClusterNotFoundException")

	err := newECSReaper(cluster.nc).Delete(testCtx(t), "000000000042",
		Resource{Kind: "ecs-cluster", ID: "gone"}, false)

	assert.NoError(t, err)
}

// A real failure must surface. Swallowing it would report the stage drained
// while the cluster is still holding tasks, ENIs and capacity.
func TestECSClusterReaperReportsARealFailure(t *testing.T) {
	cluster := newFakeECS(t)
	cluster.fail("ecs.DeleteCluster", "InternalError")

	err := newECSReaper(cluster.nc).Delete(testCtx(t), "000000000042",
		Resource{Kind: "ecs-cluster", ID: "web"}, false)

	assert.Error(t, err)
}

func TestECSClusterNameFromARN(t *testing.T) {
	assert.Equal(t, "web", ecsClusterName("arn:aws:ecs:us-west-1:000000000042:cluster/web"))
	assert.Equal(t, "web", ecsClusterName("web"))
	assert.Equal(t, "web", ecsClusterName("  web  "))
	assert.Empty(t, ecsClusterName(""))
}

// orderedCompute records when the engine reaches the instance cleanup.
type orderedCompute struct {
	ComputeStage

	order *[]string
}

func (o *orderedCompute) CleanupCompute(ctx context.Context, req *accountcleanup.ComputeRequest) (*accountcleanup.ComputeResult, error) {
	*o.order = append(*o.order, "instances")
	return o.ComputeStage.CleanupCompute(ctx, req)
}

// Container instances are ordinary EC2 instances in the tenant's account, so
// the compute cleanup terminates them — but the tasks riding on them have to be
// stopped first, so the compute-stage reapers must run before that cleanup.
func TestECSClustersAreDeletedBeforeTheInstancesAreTerminated(t *testing.T) {
	var order []string
	cluster := &fakeReaper{
		kind: "ecs-cluster", stage: StageCompute, order: &order,
		resources: []Resource{{Kind: "ecs-cluster", ID: "web"}},
	}
	accounts := newFakeAccounts()
	engine := quietEngine(accounts, cluster)
	compute, _ := cleanStages()
	engine.Compute = &orderedCompute{ComputeStage: compute, order: &order}

	_, err := engine.Teardown(context.Background(), request())

	require.NoError(t, err)
	require.Equal(t, []string{"ecs-cluster/web", "instances"}, order)
	for _, reaper := range ECSReapers(nil) {
		assert.Equal(t, StageCompute, reaper.Stage())
	}
}
