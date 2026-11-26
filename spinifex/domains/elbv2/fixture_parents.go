package elbv2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// Each VPC and subnet has one dependency slot, keyed by account and ID.
	keyPrefixOVNVPCSlot    = "ovnslot.vpc."
	keyPrefixOVNSubnetSlot = "ovnslot.subnet."

	// fixtureParentAttempts bounds each slot and parent touch loop.
	fixtureParentAttempts = 5
)

var (
	// errFixtureParentBusy reports a parent slot that another live owner holds.
	errFixtureParentBusy = errors.New("elbv2: parent is used by another load balancer")
	// errFixtureVPCUnavailable reports a VPC that is absent, marked for
	// deletion or outside the caller's account.
	errFixtureVPCUnavailable = errors.New("elbv2: VPC is not available")
	// errFixtureSubnetUnavailable is errFixtureVPCUnavailable for the subnet,
	// including a subnet in another VPC.
	errFixtureSubnetUnavailable = errors.New("elbv2: subnet is not available")
	// errFixtureSlotMalformed reports a slot value that is not a complete slot.
	errFixtureSlotMalformed = errors.New("elbv2: parent slot is malformed")
)

// fixtureSlot names the owner that depends on a parent and its fixed VPC and
// subnet pair, so a delete that invalidates the owner clears both slots.
type fixtureSlot struct {
	ClaimKey string `json:"claimKey"`
	OwnerID  string `json:"ownerId"`
	VpcID    string `json:"vpcId"`
	SubnetID string `json:"subnetId"`
}

// keys returns the owner's two slot keys, VPC first.
func (s *fixtureSlot) keys(accountID string) []string {
	return []string{vpcSlotKey(accountID, s.VpcID), subnetSlotKey(accountID, s.SubnetID)}
}

func vpcSlotKey(accountID, vpcID string) string {
	return keyPrefixOVNVPCSlot + accountID + "." + vpcID
}

func subnetSlotKey(accountID, subnetID string) string {
	return keyPrefixOVNSubnetSlot + accountID + "." + subnetID
}

// readSlot point-reads the slot at key in accountID's scope. A value with an
// unknown field, a claim outside the account, or a pair without key is malformed.
func (f *fixtureOwnerStore) readSlot(ctx context.Context, accountID, key string) (jetstream.KeyValueEntry, *fixtureSlot, error) {
	entry, err := f.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("kv get %s: %w", key, err)
	}
	var slot fixtureSlot
	dec := json.NewDecoder(bytes.NewReader(entry.Value()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&slot); err != nil {
		return nil, nil, fmt.Errorf("decode %s: %w: %w", key, errFixtureSlotMalformed, err)
	}
	if slot.OwnerID == "" || slot.VpcID == "" || slot.SubnetID == "" ||
		!strings.HasPrefix(slot.ClaimKey, LBNameKey("", accountID)) || !slices.Contains(slot.keys(accountID), key) {
		return nil, nil, fmt.Errorf("decode %s: %w", key, errFixtureSlotMalformed)
	}
	return entry, &slot, nil
}

// judgeSlot reports whether the slot's owner still holds its parents. An
// expired pending owner is invalidated by its exact revision first, so it can
// never seal later; a seal that wins that race keeps the slot live.
func (f *fixtureOwnerStore) judgeSlot(ctx context.Context, slot *fixtureSlot) (bool, error) {
	for range fixtureParentAttempts {
		entry, cur, err := f.read(ctx, slot.ClaimKey)
		if err != nil {
			return false, err
		}
		if cur == nil || cur.OwnerID != slot.OwnerID {
			return false, nil
		}
		if cur.Root.VpcId != slot.VpcID || !slices.Equal(cur.Root.Subnets, []string{slot.SubnetID}) {
			return false, fmt.Errorf("slot of %s: %w", slot.ClaimKey, errFixtureSlotMalformed)
		}
		if cur.Sealed || f.now().Sub(cur.ClaimedAt) < f.claimTTL {
			return true, nil
		}
		claim, err := newFixtureClaim(slot.ClaimKey, entry.Revision(), entry.Value())
		if err != nil {
			return false, err
		}
		switch err := f.Invalidate(ctx, claim); {
		case err == nil:
			return false, nil
		case errors.Is(err, errFixtureSealed):
			return true, nil
		case !errors.Is(err, errFixtureOwnerLost):
			return false, err
		}
	}
	return false, fmt.Errorf("elbv2: judge owner of %s exhausted retries", slot.ClaimKey)
}

// clearOwnerSlots deletes each slot that still names ownerID, at the revision
// it was read. A slot of another owner is left as it is.
func (f *fixtureOwnerStore) clearOwnerSlots(ctx context.Context, accountID, ownerID string, keys []string) error {
	for _, key := range keys {
		cleared := false
		for range fixtureParentAttempts {
			entry, slot, err := f.readSlot(ctx, accountID, key)
			if err != nil {
				return err
			}
			if slot == nil || slot.OwnerID != ownerID {
				cleared = true
				break
			}
			derr := f.kv.Delete(ctx, key, jetstream.LastRevision(entry.Revision()))
			if derr == nil {
				cleared = true
				break
			}
			if !definiteKVConflict(derr) {
				return fmt.Errorf("kv delete %s: %w", key, derr)
			}
		}
		if !cleared {
			return fmt.Errorf("elbv2: clear slot %s exhausted retries", key)
		}
	}
	return nil
}

// slotBlocks is the parent delete's dependency check for one slot. A stale
// slot is cleared with its owner's other slots; any read error fails closed.
func (f *fixtureOwnerStore) slotBlocks(ctx context.Context, accountID, key string) (bool, error) {
	for range fixtureParentAttempts {
		entry, slot, err := f.readSlot(ctx, accountID, key)
		if err != nil || slot == nil {
			return false, err
		}
		live, err := f.judgeSlot(ctx, slot)
		if err != nil || live {
			return live, err
		}
		derr := f.kv.Delete(ctx, key, jetstream.LastRevision(entry.Revision()))
		if derr == nil {
			return false, f.clearOwnerSlots(ctx, accountID, slot.OwnerID, slot.keys(accountID))
		}
		if !definiteKVConflict(derr) {
			return false, fmt.Errorf("kv delete %s: %w", key, derr)
		}
	}
	return false, fmt.Errorf("elbv2: dependency check of %s exhausted retries", key)
}

// vpcBlocksDeletion is the DeleteVpc dependency check.
func (f *fixtureOwnerStore) vpcBlocksDeletion(ctx context.Context, accountID, vpcID string) (bool, error) {
	return f.slotBlocks(ctx, accountID, vpcSlotKey(accountID, vpcID))
}

// subnetBlocksDeletion is the DeleteSubnet dependency check.
func (f *fixtureOwnerStore) subnetBlocksDeletion(ctx context.Context, accountID, subnetID string) (bool, error) {
	return f.slotBlocks(ctx, accountID, subnetSlotKey(accountID, subnetID))
}

// acquireSlot publishes slot at key. A slot of a live owner is
// errFixtureParentBusy; a stale one is taken at its read revision.
func (f *fixtureOwnerStore) acquireSlot(ctx context.Context, accountID, key string, slot fixtureSlot) error {
	value, err := json.Marshal(slot)
	if err != nil {
		return fmt.Errorf("marshal slot %s: %w", key, err)
	}
	for range fixtureParentAttempts {
		_, cerr := f.kv.Create(ctx, key, value)
		if cerr == nil {
			return nil
		}
		entry, cur, err := f.readSlot(ctx, accountID, key)
		if err != nil {
			return err
		}
		if cur == nil {
			if definiteKVConflict(cerr) {
				continue
			}
			return fmt.Errorf("kv create %s: %w", key, cerr)
		}
		if cur.OwnerID == slot.OwnerID {
			return nil
		}
		live, err := f.judgeSlot(ctx, cur)
		if err != nil {
			return err
		}
		if live {
			return errFixtureParentBusy
		}
		if _, uerr := f.kv.Update(ctx, key, value, entry.Revision()); uerr == nil {
			return nil
		}
	}
	return fmt.Errorf("elbv2: acquire slot %s exhausted retries", key)
}

// touchParent rewrites a parent at rev, re-reading it after a lost race. It
// stops when the parent is gone or the attempts are spent.
func touchParent(ctx context.Context, rev uint64, touch func(uint64) error, reread func() (uint64, error)) error {
	for attempt := 1; ; attempt++ {
		err := touch(rev)
		if !errors.Is(err, ec2vpc.ErrParentChanged) || attempt == fixtureParentAttempts {
			return err
		}
		if rev, err = reread(); err != nil {
			return err
		}
	}
}

// admitParents fences the claim's VPC and subnet: it reads both parents,
// publishes both slots, then touches the VPC and the subnet at the revisions
// read, so a delete that checked before the slots existed loses its mark.
func (f *fixtureOwnerStore) admitParents(ctx context.Context, claim *fixtureClaim) error {
	accountID, slot, err := claimSlot(claim)
	if err != nil {
		return err
	}
	vpcID, subnetID := slot.VpcID, slot.SubnetID

	readVPC := func() (uint64, error) {
		rec, rev, err := f.parents.VPC(ctx, accountID, vpcID)
		if err != nil {
			return 0, err
		}
		if rec == nil || rec.Deleting {
			return 0, errFixtureVPCUnavailable
		}
		return rev, nil
	}
	readSubnet := func() (uint64, error) {
		rec, rev, err := f.parents.Subnet(ctx, accountID, subnetID)
		if err != nil {
			return 0, err
		}
		if rec == nil || rec.Deleting || rec.VpcId != vpcID {
			return 0, errFixtureSubnetUnavailable
		}
		return rev, nil
	}
	vpcRev, err := readVPC()
	if err != nil {
		return err
	}
	subnetRev, err := readSubnet()
	if err != nil {
		return err
	}

	for _, key := range slot.keys(accountID) {
		if err := f.acquireSlot(ctx, accountID, key, slot); err != nil {
			return err
		}
	}
	if err := touchParent(ctx, vpcRev, func(rev uint64) error {
		return f.parents.TouchVPC(ctx, accountID, vpcID, rev)
	}, readVPC); err != nil {
		return err
	}
	return touchParent(ctx, subnetRev, func(rev uint64) error {
		return f.parents.TouchSubnet(ctx, accountID, subnetID, rev)
	}, readSubnet)
}

// admitAndSeal fences the parents and seals the root. A definite refusal
// invalidates the claim and then clears the claim's own slots. An unreadable
// seal result keeps the claim and slots for a retry.
func (f *fixtureOwnerStore) admitAndSeal(ctx context.Context, claim *fixtureClaim) (*LoadBalancerRecord, error) {
	err := f.admitParents(ctx, claim)
	if err == nil {
		var root *LoadBalancerRecord
		if root, err = f.Seal(ctx, claim); err == nil {
			return root, nil
		}
		if !errors.Is(err, errFixtureOwnerLost) {
			return nil, err
		}
	}
	return nil, errors.Join(err, f.abandon(ctx, claim))
}

// abandon invalidates the caller's pending claim, then clears its own slots.
// A sealed or unreadable claim keeps its slots.
func (f *fixtureOwnerStore) abandon(ctx context.Context, claim *fixtureClaim) error {
	accountID, slot, err := claimSlot(claim)
	if err != nil {
		return err
	}
	if err := f.Invalidate(ctx, claim); err != nil && !errors.Is(err, errFixtureOwnerLost) {
		return err
	}
	return f.clearOwnerSlots(ctx, accountID, slot.OwnerID, slot.keys(accountID))
}

// claimSlot derives the account and slot from the claim's persisted pending
// snapshot, the same bytes Seal publishes.
func claimSlot(claim *fixtureClaim) (string, fixtureSlot, error) {
	owner, err := claim.snapshot()
	if err != nil {
		return "", fixtureSlot{}, err
	}
	root := owner.Root
	if root.VpcId == "" || len(root.Subnets) != 1 || root.Subnets[0] == "" {
		return "", fixtureSlot{}, errors.New("elbv2: fixture root needs one VPC and one subnet")
	}
	return root.AccountID, fixtureSlot{ClaimKey: claim.Key, OwnerID: owner.OwnerID, VpcID: root.VpcId, SubnetID: root.Subnets[0]}, nil
}
