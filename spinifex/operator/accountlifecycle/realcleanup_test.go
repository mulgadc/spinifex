package accountlifecycle

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountcleanup"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountinventory"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
)

// Stores that answer as an account does once its instance record has expired:
// no instance, no outstanding teardown, no spot request, no tag residue.
type emptyComputeStores struct{}

func (emptyComputeStores) ScanAccount(context.Context, string) (*ec2spotinstance.AccountScan, error) {
	return &ec2spotinstance.AccountScan{}, nil
}

func (emptyComputeStores) CancelSpotInstanceRequests(context.Context, *ec2.CancelSpotInstanceRequestsInput, string) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

func (emptyComputeStores) AccountInstances(context.Context, string) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
}

func (emptyComputeStores) AccountTeardownOutstanding(string) ([]ec2instance.OutstandingInstance, error) {
	return nil, nil
}

func (emptyComputeStores) AccountManagedInstances(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (emptyComputeStores) ScanAccountResourceIDs(context.Context, string, ...string) ([]string, error) {
	return nil, nil
}

func (emptyComputeStores) DeleteAllTags(context.Context, string, string) error { return nil }

type heldVolumes struct{ items []accountinventory.VolumeItem }

func (h heldVolumes) VolumeInventory(context.Context, string) ([]accountinventory.VolumeItem, error) {
	return h.items, nil
}

type countingEC2 struct{ terminates, deletes atomic.Int32 }

func (c *countingEC2) TerminateInstance(context.Context, string, string) error {
	c.terminates.Add(1)
	return nil
}

func (c *countingEC2) DeleteVolume(context.Context, string, string) error {
	c.deletes.Add(1)
	return nil
}

// ADR-0003:S7. The instance's terminated record has expired and the instance is
// absent from the inventory, but its volume is still recorded in-use and
// attached. With the real compute and storage cleanup bound to these stores,
// nothing is deleted whatever Force says, the account is not removed, and the
// volume is named.
func TestS7_ExpiredTerminatedRecordAndAttachedVolumeDeleteNothingAndKeepTheAccount(t *testing.T) {
	for _, force := range []bool{false, true} {
		stores, ec2ops := emptyComputeStores{}, &countingEC2{}
		accounts := newFakeAccounts()
		engine := quietEngine(accounts)
		engine.Compute = accountcleanup.NewCompute(stores, stores, ec2ops, stores, stores)
		engine.Storage = accountcleanup.NewStorage(
			heldVolumes{items: []accountinventory.VolumeItem{{VolumeID: "vol-held", State: "in-use", AttachedInstance: "i-gone", DeleteOnTermination: true}}},
			ec2ops, stores, stores, stores, stores)
		engine.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

		req := request()
		req.Force = force
		result, err := engine.Teardown(context.Background(), req)

		if !errors.Is(err, ErrResourcesStuck) {
			t.Fatalf("force=%v: a held volume must end the teardown stuck, got %v", force, err)
		}
		if ec2ops.deletes.Load() != 0 || ec2ops.terminates.Load() != 0 {
			t.Fatalf("force=%v: nothing may be deleted or terminated, deletes=%d terminates=%d", force, ec2ops.deletes.Load(), ec2ops.terminates.Load())
		}
		if accounts.deleted {
			t.Fatalf("force=%v: the account must not be removed", force)
		}
		named := false
		for _, stage := range result.Stages {
			for _, s := range stage.Stuck {
				named = named || s.Resource.ID == "vol-held"
			}
		}
		if !named {
			t.Fatalf("force=%v: the stuck volume must be named in the result", force)
		}
	}
}
