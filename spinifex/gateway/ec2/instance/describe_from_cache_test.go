package gateway_ec2_instance_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCache is a gateway_ec2_instance.CacheReader stand-in: a fixed instance
// set plus independently drivable ready/degraded flags, so a test can assert
// the not-ready case and the degraded-but-answering case separately.
type fakeCache struct {
	byID     map[string]*vm.VM
	ready    bool
	degraded bool
}

// List mirrors instancecache.Cache.List's own account-index restriction: only
// entries this exact account owns come back, never another account's.
func (f *fakeCache) List(_ context.Context, accountID string) ([]*vm.VM, bool) {
	var out []*vm.VM
	for _, v := range f.byID {
		if v.AccountID == accountID {
			out = append(out, v)
		}
	}
	return out, f.ready
}

func (f *fakeCache) Get(_ context.Context, instanceID string) (*vm.VM, error) {
	return f.byID[instanceID], nil
}

func (f *fakeCache) Degraded() bool { return f.degraded }

const cacheTestAccount = "123456789012"

// cacheVM builds a record as the cache would hand it over: a non-nil
// Reservation/Instance, since Reservations() skips an entry missing either.
func cacheVM(id, accountID string, status vm.InstanceState, desired vm.DesiredState) *vm.VM {
	return &vm.VM{
		ID:           id,
		AccountID:    accountID,
		Status:       status,
		DesiredState: desired,
		Reservation:  &ec2.Reservation{ReservationId: aws.String("r-" + id), OwnerId: aws.String(accountID)},
		Instance:     &ec2.Instance{InstanceId: aws.String(id)},
	}
}

func instanceIDs(out *ec2.DescribeInstancesOutput) []string {
	var ids []string
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			ids = append(ids, aws.StringValue(inst.InstanceId))
		}
	}
	return ids
}

// A nil cache reader means no cache is wired; DescribeFromCache must report
// no answer rather than panic or fabricate an empty result.
func TestDescribeFromCache_NilCache_NoAnswer(t *testing.T) {
	out, degraded, err := gateway_ec2_instance.DescribeFromCache(
		context.Background(), nil, &ec2.DescribeInstancesInput{}, cacheTestAccount, "us-east-1a")

	require.NoError(t, err)
	assert.False(t, degraded)
	assert.Nil(t, out, "a nil cache must answer with no answer, not an empty list")
}

// A cache that has not completed its first whole-set sync cannot support a
// claim about the account's whole set, so a filters-only request (no
// InstanceIds, so no per-key Get can settle it instead) must get no answer,
// distinctly from a genuinely empty account.
func TestDescribeFromCache_NotReady_NoAnswer(t *testing.T) {
	cache := &fakeCache{
		byID:  map[string]*vm.VM{"i-1": cacheVM("i-1", cacheTestAccount, vm.StateRunning, vm.DesiredRunning)},
		ready: false,
	}

	out, _, err := gateway_ec2_instance.DescribeFromCache(
		context.Background(), cache, &ec2.DescribeInstancesInput{}, cacheTestAccount, "us-east-1a")

	require.NoError(t, err)
	assert.Nil(t, out, "a not-ready cache must answer with no answer, not an empty list")
}

// A ready cache with nothing for this account is a real, empty answer — the
// two must stay distinguishable from the not-ready case above.
func TestDescribeFromCache_ReadyButEmpty_EmptyAnswerNotNoAnswer(t *testing.T) {
	cache := &fakeCache{byID: map[string]*vm.VM{}, ready: true}

	out, _, err := gateway_ec2_instance.DescribeFromCache(
		context.Background(), cache, &ec2.DescribeInstancesInput{}, cacheTestAccount, "us-east-1a")

	require.NoError(t, err)
	require.NotNil(t, out, "a ready cache with nothing for this account is an empty answer, not no answer")
	assert.Empty(t, out.Reservations)
}

// An explicit InstanceIds request resolves through Get, which answers per key
// regardless of whether the whole-set sync has completed — a request that
// names its instances must not be held hostage by List's readiness gate.
func TestDescribeFromCache_ExplicitID_AnswersDespiteNotReady(t *testing.T) {
	cache := &fakeCache{
		byID:  map[string]*vm.VM{"i-1": cacheVM("i-1", cacheTestAccount, vm.StateRunning, vm.DesiredRunning)},
		ready: false,
	}

	input := &ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String("i-1")}}
	out, _, err := gateway_ec2_instance.DescribeFromCache(context.Background(), cache, input, cacheTestAccount, "us-east-1a")

	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, []string{"i-1"}, instanceIDs(out))
}

// A terminated record must never surface: the unified record space keeps one
// around indefinitely when its owning node never retired it, but neither the
// fan-out nor the (separate) terminated KV bucket would ever return it.
func TestDescribeFromCache_ExcludesTerminated(t *testing.T) {
	cache := &fakeCache{
		byID: map[string]*vm.VM{
			"i-run":  cacheVM("i-run", cacheTestAccount, vm.StateRunning, vm.DesiredRunning),
			"i-term": cacheVM("i-term", cacheTestAccount, vm.StateTerminated, vm.DesiredRunning),
		},
		ready: true,
	}

	out, _, err := gateway_ec2_instance.DescribeFromCache(
		context.Background(), cache, &ec2.DescribeInstancesInput{}, cacheTestAccount, "us-east-1a")

	require.NoError(t, err)
	assert.Equal(t, []string{"i-run"}, instanceIDs(out))
}

// An operator-stopped instance (status=Stopped, desired=Stopped) is exactly
// what the stopped KV bucket's own predicate returns, and must be included —
// classified via vm.OperatorStopped rather than a re-derived rule.
func TestDescribeFromCache_IncludesOperatorStoppedInstance(t *testing.T) {
	stopped := cacheVM("i-stopped", cacheTestAccount, vm.StateStopped, vm.DesiredStopped)
	require.True(t, stopped.OperatorStopped())
	cache := &fakeCache{byID: map[string]*vm.VM{"i-stopped": stopped}, ready: true}

	out, _, err := gateway_ec2_instance.DescribeFromCache(
		context.Background(), cache, &ec2.DescribeInstancesInput{}, cacheTestAccount, "us-east-1a")

	require.NoError(t, err)
	assert.Equal(t, []string{"i-stopped"}, instanceIDs(out))
}

// A drain-stopped instance (status=Stopped, desired=Running) is neither an
// operator-stopped record the stopped bucket would return, nor is any node's
// vmMgr reporting it (that node stopped the process as part of its own
// drain) — today it is invisible everywhere, and the cache path must match.
func TestDescribeFromCache_ExcludesDrainStoppedInstance(t *testing.T) {
	drained := cacheVM("i-drained", cacheTestAccount, vm.StateStopped, vm.DesiredRunning)
	require.False(t, drained.OperatorStopped())
	cache := &fakeCache{byID: map[string]*vm.VM{"i-drained": drained}, ready: true}

	out, _, err := gateway_ec2_instance.DescribeFromCache(
		context.Background(), cache, &ec2.DescribeInstancesInput{}, cacheTestAccount, "us-east-1a")

	require.NoError(t, err)
	assert.Empty(t, out.Reservations)
}

// StatePending, StateError and StateShuttingDown carry no exclusion in the
// per-node fan-out responder (InstanceServiceImpl.DescribeInstances iterates
// vmMgr with no state-based filter beyond visibility/ID/filters), so the
// cache path must include them the same way.
func TestDescribeFromCache_IncludesPendingErrorShuttingDown(t *testing.T) {
	for _, state := range []vm.InstanceState{vm.StatePending, vm.StateError, vm.StateShuttingDown, vm.StateProvisioning, vm.StateStopping, vm.StateRunning} {
		t.Run(string(state), func(t *testing.T) {
			cache := &fakeCache{
				byID:  map[string]*vm.VM{"i-1": cacheVM("i-1", cacheTestAccount, state, vm.DesiredRunning)},
				ready: true,
			}

			out, _, err := gateway_ec2_instance.DescribeFromCache(
				context.Background(), cache, &ec2.DescribeInstancesInput{}, cacheTestAccount, "us-east-1a")

			require.NoError(t, err)
			assert.Equal(t, []string{"i-1"}, instanceIDs(out), "state %s must be included, matching the fan-out", state)
		})
	}
}

// Degraded() is surfaced on a real answer for a later stage to act on; it
// must not itself suppress or alter the result in this stage.
func TestDescribeFromCache_SurfacesDegradedWithoutActingOnIt(t *testing.T) {
	cache := &fakeCache{
		byID:     map[string]*vm.VM{"i-1": cacheVM("i-1", cacheTestAccount, vm.StateRunning, vm.DesiredRunning)},
		ready:    true,
		degraded: true,
	}

	out, degraded, err := gateway_ec2_instance.DescribeFromCache(
		context.Background(), cache, &ec2.DescribeInstancesInput{}, cacheTestAccount, "us-east-1a")

	require.NoError(t, err)
	assert.True(t, degraded)
	assert.Equal(t, []string{"i-1"}, instanceIDs(out), "degraded must be surfaced, not acted on, this stage")
}

// A malformed filter is a deterministic client error the cache path must
// surface itself rather than silently answering around it.
func TestDescribeFromCache_InvalidFilterReturnsError(t *testing.T) {
	cache := &fakeCache{ready: true, byID: map[string]*vm.VM{}}
	input := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{{Name: aws.String("bogus-filter"), Values: []*string{aws.String("x")}}},
	}

	out, _, err := gateway_ec2_instance.DescribeFromCache(context.Background(), cache, input, cacheTestAccount, "us-east-1a")

	require.Error(t, err)
	assert.Nil(t, out)
}

// The cache path (DescribeFromCache) and the KV path (Reservations, the same
// call describeInstancesFromKV makes) must agree on visibility for a
// ManagedBy instance: both share ParseInstanceListSelection/Reservations, so
// neither can apply the hide rule differently.
func TestDescribeFromCache_AgreesWithKVPathOnManagedByVisibility(t *testing.T) {
	owner := "111122223333"
	managed := cacheVM("i-ekscp", owner, vm.StateStopped, vm.DesiredStopped)
	managed.ManagedBy = tags.ManagedByEKS

	cacheOut, _, err := gateway_ec2_instance.DescribeFromCache(
		context.Background(), &fakeCache{byID: map[string]*vm.VM{"i-ekscp": managed}, ready: true},
		&ec2.DescribeInstancesInput{}, owner, "us-east-1a")
	require.NoError(t, err)

	sel, err := handlers_ec2_instance.ParseInstanceListSelection(context.Background(),
		&ec2.DescribeInstancesInput{}, owner, "DescribeStoppedInstances")
	require.NoError(t, err)
	kvOut := sel.Reservations(context.Background(), []*vm.VM{managed}, "us-east-1a", 80, "stopped")

	assert.Empty(t, cacheOut.Reservations, "cache path must hide the managed system VM from its own account")
	assert.Empty(t, kvOut.Reservations, "KV path must hide it too — the two must never disagree")

	// Owned by Global (LB/EKS control-plane VMs are system-account-owned):
	// both paths let the Global caller through.
	systemOwned := cacheVM("i-lb", utils.GlobalAccountID, vm.StateStopped, vm.DesiredStopped)
	systemOwned.ManagedBy = tags.ManagedByELBv2

	cacheOutGlobal, _, err := gateway_ec2_instance.DescribeFromCache(
		context.Background(), &fakeCache{byID: map[string]*vm.VM{"i-lb": systemOwned}, ready: true},
		&ec2.DescribeInstancesInput{}, utils.GlobalAccountID, "us-east-1a")
	require.NoError(t, err)

	selGlobal, err := handlers_ec2_instance.ParseInstanceListSelection(context.Background(),
		&ec2.DescribeInstancesInput{}, utils.GlobalAccountID, "DescribeStoppedInstances")
	require.NoError(t, err)
	kvOutGlobal := selGlobal.Reservations(context.Background(), []*vm.VM{systemOwned}, "us-east-1a", 80, "stopped")

	assert.Equal(t, []string{"i-lb"}, instanceIDs(cacheOutGlobal), "cache path must show Global its own system VM")
	assert.Equal(t, []string{"i-lb"}, instanceIDs(kvOutGlobal), "KV path must agree")
}
