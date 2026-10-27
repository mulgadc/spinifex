//test:in-package — the listers are unexported composition-root adapters with
// no production caller to reach them through yet.

package awsgw

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	elbv2domain "github.com/mulgadc/spinifex/spinifex/domains/elbv2"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/tagging"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
)

const (
	listerRegion   = "ap-southeast-2"
	listerAccount  = "111122223333"
	foreignAccount = "444455556666"
)

func serveSubject(t *testing.T, nc *nats.Conn, subject string, fn func(*nats.Msg)) {
	t.Helper()
	sub, err := nc.Subscribe(subject, fn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// failSubject answers every request on subject with err, as an owner would.
func failSubject(t *testing.T, nc *nats.Conn, subject string, err error) {
	t.Helper()
	serveSubject(t, nc, subject, func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(context.Context, *map[string]any) (*struct{}, error) { return nil, err })
	})
}

func byARN(t *testing.T, got []tagging.TaggedResource) map[string]map[string]string {
	t.Helper()
	out := make(map[string]map[string]string, len(got))
	for _, r := range got {
		_, dup := out[r.ARN]
		require.False(t, dup, "ARN listed twice: %s", r.ARN)
		out[r.ARN] = r.Tags
	}
	return out
}

func sortedARNs(got []tagging.TaggedResource) []string {
	out := make([]string, 0, len(got))
	for _, r := range got {
		out = append(out, r.ARN)
	}
	slices.Sort(out)
	return out
}

// newEC2IndexFixture serves ec2.DescribeTags from a real tag index on an
// embedded JetStream and returns the index for seeding.
func newEC2IndexFixture(t *testing.T) (*nats.Conn, *ec2tags.TagsServiceImpl) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := ec2tags.GetOrCreateTagsBucket(t.Context(), js)
	require.NoError(t, err)
	svc := ec2tags.NewTagsServiceImplWithStore(&config.Config{}, nil, kv)
	serveSubject(t, nc, "ec2.DescribeTags", func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, in *ec2.DescribeTagsInput) (*ec2.DescribeTagsOutput, error) {
			return svc.DescribeTags(ctx, in, natsmsg.AccountIDFromMsg(msg))
		})
	})
	return nc, svc
}

func seedEC2Tags(t *testing.T, svc *ec2tags.TagsServiceImpl, accountID string, ids []string, tags map[string]string) {
	t.Helper()
	in := &ec2.CreateTagsInput{Resources: aws.StringSlice(ids)}
	for k, v := range tags {
		in.Tags = append(in.Tags, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	_, err := svc.CreateTags(t.Context(), in, accountID)
	require.NoError(t, err)
}

func TestEC2IndexLister_ListsCallerIndexEntriesAsRegionalARNs(t *testing.T) {
	nc, svc := newEC2IndexFixture(t)
	seedEC2Tags(t, svc, listerAccount, []string{"sg-0000000000000000a"}, map[string]string{"env": "prod", "Name": "web"})
	seedEC2Tags(t, svc, listerAccount, []string{"subnet-0000000000000000b"}, map[string]string{"env": "dev"})
	seedEC2Tags(t, svc, listerAccount, []string{"vol-00000000000000000"}, map[string]string{"env": "orphan"})
	// CreateTags rejects an unrecognised prefix, but an entry stored before it
	// did is still in the index and still listed.
	require.NoError(t, svc.PutResourceTags(t.Context(), listerAccount, "not-a-real-id", map[string]string{"env": "orphan"}))
	seedEC2Tags(t, svc, foreignAccount, []string{"sg-0000000000000000f"}, map[string]string{"env": "other"})

	got, err := newEC2IndexLister(nc, listerRegion).ListTagged(t.Context(), listerAccount, nil)
	require.NoError(t, err)

	// Unresolved IDs are listed: an unrecognised prefix as type "unknown", a
	// well-formed ID naming no resource as that resource's type.
	prefix := "arn:aws:ec2:" + listerRegion + ":" + listerAccount + ":"
	assert.Equal(t, map[string]map[string]string{
		prefix + "security-group/sg-0000000000000000a": {"env": "prod", "Name": "web"},
		prefix + "subnet/subnet-0000000000000000b":     {"env": "dev"},
		prefix + "unknown/not-a-real-id":               {"env": "orphan"},
		prefix + "volume/vol-00000000000000000":        {"env": "orphan"},
	}, byARN(t, got))
}

func TestEC2IndexLister_TypeFilters(t *testing.T) {
	nc, svc := newEC2IndexFixture(t)
	seedEC2Tags(t, svc, listerAccount, []string{"sg-0000000000000000a"}, map[string]string{"env": "prod"})
	seedEC2Tags(t, svc, listerAccount, []string{"subnet-0000000000000000b"}, map[string]string{"env": "prod"})
	lister := newEC2IndexLister(nc, listerRegion)
	prefix := "arn:aws:ec2:" + listerRegion + ":" + listerAccount + ":"

	for _, tc := range []struct {
		filters map[string]bool
		want    []string
	}{
		{map[string]bool{"ec2": true}, []string{prefix + "security-group/sg-0000000000000000a", prefix + "subnet/subnet-0000000000000000b"}},
		{map[string]bool{"ec2:subnet": true}, []string{prefix + "subnet/subnet-0000000000000000b"}},
		{map[string]bool{"ec2:subnet": true, "ec2:security-group": true}, []string{prefix + "security-group/sg-0000000000000000a", prefix + "subnet/subnet-0000000000000000b"}},
		{map[string]bool{"elasticloadbalancing": true}, []string{}},
		{map[string]bool{"ec2:instance": true}, []string{}},
	} {
		t.Run(fmt.Sprint(tc.filters), func(t *testing.T) {
			got, err := lister.ListTagged(t.Context(), listerAccount, tc.filters)
			require.NoError(t, err)
			assert.Equal(t, tc.want, sortedARNs(got))
		})
	}
}

func TestEC2IndexLister_ReturnsFreshTagMapsPerCall(t *testing.T) {
	nc, svc := newEC2IndexFixture(t)
	seedEC2Tags(t, svc, listerAccount, []string{"sg-0000000000000000a"}, map[string]string{"env": "prod"})
	lister := newEC2IndexLister(nc, listerRegion)

	first, err := lister.ListTagged(t.Context(), listerAccount, nil)
	require.NoError(t, err)
	require.Len(t, first, 1)
	first[0].Tags["env"] = "mutated"

	second, err := lister.ListTagged(t.Context(), listerAccount, nil)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, map[string]string{"env": "prod"}, second[0].Tags)
}

func TestEC2IndexLister_PropagatesOwnerError(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	failSubject(t, nc, "ec2.DescribeTags", errors.New(awserrors.ErrorServerInternal))

	got, err := newEC2IndexLister(nc, listerRegion).ListTagged(t.Context(), listerAccount, nil)
	require.EqualError(t, err, awserrors.ErrorServerInternal)
	assert.Nil(t, got)
}

func TestEC2TaggedResources(t *testing.T) {
	tags := []*ec2.TagDescription{
		{ResourceId: aws.String("subnet-1"), ResourceType: aws.String("subnet"), Key: aws.String("Name"), Value: aws.String("a")},
		{ResourceId: aws.String("subnet-1"), ResourceType: aws.String("subnet"), Key: aws.String("env"), Value: aws.String("prod")},
		{ResourceId: aws.String("sg-1"), ResourceType: aws.String("security-group"), Key: aws.String("Name"), Value: aws.String("b")},
		{ResourceId: aws.String(""), ResourceType: aws.String("subnet"), Key: aws.String("skip"), Value: aws.String("me")},
	}

	// No type filter: both resources, tags merged per resource, empty ID skipped.
	all := ec2TaggedResources(tags, "us-east-1", "123456789012", nil)
	assert.Equal(t, map[string]map[string]string{
		"arn:aws:ec2:us-east-1:123456789012:subnet/subnet-1":     {"Name": "a", "env": "prod"},
		"arn:aws:ec2:us-east-1:123456789012:security-group/sg-1": {"Name": "b"},
	}, byARN(t, all))

	// Type filter narrows to subnets only.
	onlySubnets := ec2TaggedResources(tags, "us-east-1", "123456789012", map[string]bool{"ec2:subnet": true})
	assert.Equal(t, []string{"arn:aws:ec2:us-east-1:123456789012:subnet/subnet-1"}, sortedARNs(onlySubnets))
}

func TestEC2ARN(t *testing.T) {
	assert.Equal(t,
		"arn:aws:ec2:us-east-1:123456789012:subnet/subnet-abc",
		ec2ARN("us-east-1", "123456789012", "ec2:subnet", "subnet-abc"))
}

// elbv2Fixture serves the ELBv2 read subjects from a real ELBv2 service on an
// embedded JetStream, recording the ARN count of every DescribeTags request.
type elbv2Fixture struct {
	nc    *nats.Conn
	store *elbv2domain.Store

	mu            sync.Mutex
	describeCalls int
	tagBatches    []int
}

func newELBv2Fixture(t *testing.T) *elbv2Fixture {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	svc, err := elbv2domain.NewELBv2ServiceImplWithNATS(nil, nc, masterKey)
	require.NoError(t, err)
	t.Cleanup(svc.Close)
	store, err := elbv2domain.NewStore(t.Context(), nc)
	require.NoError(t, err)

	f := &elbv2Fixture{nc: nc, store: store}
	serveSubject(t, nc, "elbv2.DescribeLoadBalancers", func(msg *nats.Msg) {
		f.countDescribe()
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, in *elbv2.DescribeLoadBalancersInput) (*elbv2.DescribeLoadBalancersOutput, error) {
			return svc.DescribeLoadBalancers(ctx, in, natsmsg.AccountIDFromMsg(msg))
		})
	})
	serveSubject(t, nc, "elbv2.DescribeTargetGroups", func(msg *nats.Msg) {
		f.countDescribe()
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, in *elbv2.DescribeTargetGroupsInput) (*elbv2.DescribeTargetGroupsOutput, error) {
			return svc.DescribeTargetGroups(ctx, in, natsmsg.AccountIDFromMsg(msg))
		})
	})
	serveSubject(t, nc, "elbv2.DescribeTags", func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, in *elbv2.DescribeTagsInput) (*elbv2.DescribeTagsOutput, error) {
			f.mu.Lock()
			f.tagBatches = append(f.tagBatches, len(in.ResourceArns))
			f.mu.Unlock()
			return svc.DescribeTags(ctx, in, natsmsg.AccountIDFromMsg(msg))
		})
	})
	return f
}

func (f *elbv2Fixture) countDescribe() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describeCalls++
}

func (f *elbv2Fixture) calls() (describes int, batches []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.describeCalls, slices.Clone(f.tagBatches)
}

func (f *elbv2Fixture) putLB(t *testing.T, accountID, name, id string, tags map[string]string) string {
	t.Helper()
	lbARN := arn.FormatELBv2LoadBalancer(listerRegion, accountID, name, id, "application")
	require.NoError(t, f.store.PutLoadBalancer(t.Context(), &elbv2domain.LoadBalancerRecord{
		LoadBalancerArn: lbARN, LoadBalancerID: id, Name: name, Type: "application", AccountID: accountID, Tags: tags,
	}))
	return lbARN
}

func (f *elbv2Fixture) putTG(t *testing.T, accountID, name, id string, tags map[string]string) string {
	t.Helper()
	tgARN := arn.FormatELBv2TargetGroup(listerRegion, accountID, name, id)
	require.NoError(t, f.store.PutTargetGroup(t.Context(), &elbv2domain.TargetGroupRecord{
		TargetGroupArn: tgARN, TargetGroupID: id, Name: name, AccountID: accountID, Tags: tags,
	}))
	return tgARN
}

func TestELBv2Lister_ListsCallerLoadBalancersAndTargetGroupsTaggedOrNot(t *testing.T) {
	f := newELBv2Fixture(t)
	taggedLB := f.putLB(t, listerAccount, "tagged", "00000000000000a1", map[string]string{"env": "prod", "team": "web"})
	untaggedLB := f.putLB(t, listerAccount, "untagged", "00000000000000a2", nil)
	taggedTG := f.putTG(t, listerAccount, "tg", "00000000000000b1", map[string]string{"env": "prod"})
	f.putLB(t, foreignAccount, "foreign", "00000000000000c1", map[string]string{"env": "other"})
	f.putTG(t, foreignAccount, "foreign-tg", "00000000000000c2", nil)

	got, err := newELBv2Lister(f.nc).ListTagged(t.Context(), listerAccount, nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]map[string]string{
		taggedLB:   {"env": "prod", "team": "web"},
		untaggedLB: {},
		taggedTG:   {"env": "prod"},
	}, byARN(t, got))
}

func TestELBv2Lister_TypeFilters(t *testing.T) {
	f := newELBv2Fixture(t)
	lb := f.putLB(t, listerAccount, "lb", "00000000000000a1", map[string]string{"env": "prod"})
	tg := f.putTG(t, listerAccount, "tg", "00000000000000b1", map[string]string{"env": "prod"})
	lister := newELBv2Lister(f.nc)

	for _, tc := range []struct {
		filters      map[string]bool
		want         []string
		wantDescribe int
	}{
		{map[string]bool{"elasticloadbalancing": true}, []string{lb, tg}, 2},
		{map[string]bool{"elasticloadbalancing:loadbalancer": true}, []string{lb}, 1},
		{map[string]bool{"elasticloadbalancing:targetgroup": true}, []string{tg}, 1},
		// Neither served type admitted: no owner call is made.
		{map[string]bool{"elasticloadbalancing:listener": true}, []string{}, 0},
		{map[string]bool{"ec2": true}, []string{}, 0},
	} {
		t.Run(fmt.Sprint(tc.filters), func(t *testing.T) {
			before, _ := f.calls()
			got, err := lister.ListTagged(t.Context(), listerAccount, tc.filters)
			require.NoError(t, err)
			assert.Equal(t, tc.want, sortedARNs(got))
			after, _ := f.calls()
			assert.Equal(t, tc.wantDescribe, after-before)
		})
	}
}

func TestELBv2Lister_BatchesDescribeTagsByTwenty(t *testing.T) {
	f := newELBv2Fixture(t)
	want := make([]string, 0, 45)
	for i := range 45 {
		want = append(want, f.putLB(t, listerAccount, fmt.Sprintf("lb-%02d", i), fmt.Sprintf("%016x", i+1), map[string]string{"n": strconv.Itoa(i)}))
	}
	slices.Sort(want)

	got, err := newELBv2Lister(f.nc).ListTagged(t.Context(), listerAccount, map[string]bool{"elasticloadbalancing:loadbalancer": true})
	require.NoError(t, err)
	assert.Equal(t, want, sortedARNs(got))
	_, batches := f.calls()
	assert.Equal(t, []int{20, 20, 5}, batches)
}

func TestELBv2Lister_PropagatesOwnerErrors(t *testing.T) {
	for _, subject := range []string{"elbv2.DescribeLoadBalancers", "elbv2.DescribeTargetGroups", "elbv2.DescribeTags"} {
		t.Run(subject, func(t *testing.T) {
			_, nc, _ := testutil.StartTestJetStream(t)
			ok := func(out any) func(*nats.Msg) {
				return func(msg *nats.Msg) {
					natsmsg.ServeNATSRequestCtx(msg, func(context.Context, *map[string]any) (*any, error) { return &out, nil })
				}
			}
			lbs := &elbv2.DescribeLoadBalancersOutput{LoadBalancers: []*elbv2.LoadBalancer{{LoadBalancerArn: aws.String(arn.FormatELBv2LoadBalancer(listerRegion, listerAccount, "lb", "00000000000000a1", "application"))}}}
			handlers := map[string]func(*nats.Msg){
				"elbv2.DescribeLoadBalancers": ok(lbs),
				"elbv2.DescribeTargetGroups":  ok(&elbv2.DescribeTargetGroupsOutput{}),
				"elbv2.DescribeTags":          ok(&elbv2.DescribeTagsOutput{}),
			}
			for s, h := range handlers {
				if s == subject {
					failSubject(t, nc, s, errors.New(awserrors.ErrorServerInternal))
					continue
				}
				serveSubject(t, nc, s, h)
			}

			got, err := newELBv2Lister(nc).ListTagged(t.Context(), listerAccount, nil)
			require.EqualError(t, err, awserrors.ErrorServerInternal)
			assert.Nil(t, got)
		})
	}
}
