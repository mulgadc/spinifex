package accountteardown

//test:in-package — the reapers are unexported, and the pagination and ordering
// rules asserted here are the substance of them.

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEKS answers the eks.* subjects. Replies can be queued per subject so a
// paginated listing can be scripted across successive calls.
type fakeEKS struct {
	nc *nats.Conn

	mu        sync.Mutex
	requests  map[string][][]byte
	queued    map[string][]any
	errorCode map[string]string
}

func newFakeEKS(t *testing.T) *fakeEKS {
	t.Helper()
	_, nc := testutil.StartTestNATS(t)

	fake := &fakeEKS{
		nc:        nc,
		requests:  map[string][][]byte{},
		queued:    map[string][]any{},
		errorCode: map[string]string{},
	}

	sub, err := nc.Subscribe("eks.>", fake.serve)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	return fake
}

func (f *fakeEKS) serve(msg *nats.Msg) {
	f.mu.Lock()
	f.requests[msg.Subject] = append(f.requests[msg.Subject], msg.Data)
	code, isError := f.errorCode[msg.Subject]

	var reply any
	hasReply := false
	if queue := f.queued[msg.Subject]; len(queue) > 0 {
		reply = queue[0]
		hasReply = true
		// The last queued reply repeats, so a drain loop that lists once more
		// than the test scripted does not fall off the end.
		if len(queue) > 1 {
			f.queued[msg.Subject] = queue[1:]
		}
	}
	f.mu.Unlock()

	switch {
	case isError:
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

func (f *fakeEKS) reply(subject string, outputs ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued[subject] = append(f.queued[subject], outputs...)
}

func (f *fakeEKS) fail(subject, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errorCode[subject] = code
}

func (f *fakeEKS) requestCount(subject string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests[subject])
}

func (f *fakeEKS) requestAt(t *testing.T, subject string, index int, into any) {
	t.Helper()
	f.mu.Lock()
	payloads := f.requests[subject]
	f.mu.Unlock()
	require.Greater(t, len(payloads), index, "subject %s was called %d times", subject, len(payloads))
	require.NoError(t, json.Unmarshal(payloads[index], into))
}

func newEKSReapers(nc *nats.Conn) (*eksNodegroupReaper, *eksClusterReaper) {
	svc := handlers_eks.NewNATSEKSService(nc)
	return &eksNodegroupReaper{svc: svc}, &eksClusterReaper{svc: svc}
}

func TestEKSClusterReaperListsEveryCluster(t *testing.T) {
	fake := newFakeEKS(t)
	fake.reply("eks.ListClusters", eks.ListClustersOutput{
		Clusters: aws.StringSlice([]string{"alpha", "beta"}),
	})

	_, clusters := newEKSReapers(fake.nc)
	found, err := clusters.List(testCtx(t), "000000000042")
	require.NoError(t, err)

	assert.Equal(t, []Resource{
		{Kind: "eks-cluster", ID: "alpha"},
		{Kind: "eks-cluster", ID: "beta"},
	}, found)
}

// ListClusters caps a page at 100. Reading only the first page would report
// the stage drained with clusters still standing, and a teardown that misses
// one never converges.
func TestEKSClusterReaperFollowsPagination(t *testing.T) {
	fake := newFakeEKS(t)
	fake.reply("eks.ListClusters",
		eks.ListClustersOutput{Clusters: aws.StringSlice([]string{"alpha"}), NextToken: aws.String("beta")},
		eks.ListClustersOutput{Clusters: aws.StringSlice([]string{"beta"})},
	)

	_, clusters := newEKSReapers(fake.nc)
	found, err := clusters.List(testCtx(t), "000000000042")
	require.NoError(t, err)

	require.Len(t, found, 2)
	assert.Equal(t, "beta", found[1].ID)

	var second eks.ListClustersInput
	fake.requestAt(t, "eks.ListClusters", 1, &second)
	assert.Equal(t, "beta", aws.StringValue(second.NextToken))
}

// A NextToken that never advances would otherwise spin the drain loop for the
// whole stage budget without listing anything new.
func TestEKSClusterReaperStopsOnARepeatingPageToken(t *testing.T) {
	fake := newFakeEKS(t)
	fake.reply("eks.ListClusters", eks.ListClustersOutput{
		Clusters:  aws.StringSlice([]string{"alpha"}),
		NextToken: aws.String("alpha"),
	})

	_, clusters := newEKSReapers(fake.nc)
	found, err := clusters.List(testCtx(t), "000000000042")
	require.NoError(t, err)

	assert.Len(t, found, eksListPageLimit)
	assert.Equal(t, eksListPageLimit, fake.requestCount("eks.ListClusters"))
}

// A nodegroup name is unique only within its cluster, so the pair is carried
// through the listing and split again on delete.
func TestEKSNodegroupReaperCarriesTheClusterWithTheName(t *testing.T) {
	fake := newFakeEKS(t)
	fake.reply("eks.ListClusters", eks.ListClustersOutput{Clusters: aws.StringSlice([]string{"alpha"})})
	fake.reply("eks.ListNodegroups", eks.ListNodegroupsOutput{
		Nodegroups: aws.StringSlice([]string{"workers"}),
	})

	nodegroups, _ := newEKSReapers(fake.nc)
	found, err := nodegroups.List(testCtx(t), "000000000042")
	require.NoError(t, err)
	require.Equal(t, []Resource{{Kind: "eks-nodegroup", ID: "alpha/workers"}}, found)

	require.NoError(t, nodegroups.Delete(testCtx(t), "000000000042", found[0], false))

	var input eks.DeleteNodegroupInput
	fake.requestAt(t, "eks.DeleteNodegroup", 0, &input)
	assert.Equal(t, "alpha", aws.StringValue(input.ClusterName))
	assert.Equal(t, "workers", aws.StringValue(input.NodegroupName))
}

// A cluster deleted between the two calls is the ordinary result of a
// concurrent teardown, not a reason to fail the whole listing.
func TestEKSNodegroupReaperSkipsAClusterThatWentAway(t *testing.T) {
	fake := newFakeEKS(t)
	fake.reply("eks.ListClusters", eks.ListClustersOutput{Clusters: aws.StringSlice([]string{"alpha"})})
	fake.fail("eks.ListNodegroups", "ResourceNotFoundException")

	nodegroups, _ := newEKSReapers(fake.nc)
	found, err := nodegroups.List(testCtx(t), "000000000042")

	require.NoError(t, err)
	assert.Empty(t, found)
}

func TestEKSClusterReaperTreatsAMissingClusterAsDeleted(t *testing.T) {
	fake := newFakeEKS(t)
	fake.fail("eks.DeleteCluster", "ResourceNotFoundException")

	_, clusters := newEKSReapers(fake.nc)
	err := clusters.Delete(testCtx(t), "000000000042", Resource{Kind: "eks-cluster", ID: "gone"}, false)

	assert.NoError(t, err)
}

// DeleteCluster tears down the NLB, the control-plane VMs and the managed CP
// VPC but never touches nodegroups. Deleting the cluster first would strand
// every worker node with nothing left to address it by.
func TestEKSNodegroupsAreReapedBeforeClusters(t *testing.T) {
	reapers := EKSReapers(nil)

	assert.Less(t, indexOfKind(reapers, "eks-nodegroup"), indexOfKind(reapers, "eks-cluster"))
	for _, reaper := range reapers {
		assert.Equal(t, StageCompute, reaper.Stage(), "%s runs in the compute stage", reaper.Kind())
	}
}
