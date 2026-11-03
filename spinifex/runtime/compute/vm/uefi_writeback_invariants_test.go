package vm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// s7Clause is quoted by every failure here: pruning and the write-back reaper
// delete customer data on their own, which the clause allows only for an
// object no record points at. Fix the code, not the assertion.
const s7Clause = "ADR-0003:S7: a reaper's judgement is never the decision that deletes customer data; " +
	"fix runtime/compute/vm/uefi_writeback*.go so the pointed store survives"

// A copy that is fenced, errored or superseded writes nothing back: no object
// is put, the pointer every record carries stays where it was, no obligation
// is recorded, and each skip is counted under its own reason.
func TestS7_UEFISkippedWriteBackNeverMovesThePointer(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		setup  func(t *testing.T, c *wbCluster, a *wbNode, v *VM)
		stop   func(t *testing.T, a *wbNode, v *VM)
	}{
		{
			name: "the record names another node", reason: "superseded",
			setup: func(_ *testing.T, c *wbCluster, _ *wbNode, v *VM) { c.pointers.run(nodeB, v) },
		},
		{
			name: "seeded.json names an older pointer", reason: "reseeded",
			setup: func(t *testing.T, _ *wbCluster, a *wbNode, _ *VM) {
				require.NoError(t, writeUEFISeed(a.dir(), keyFor("older")))
			},
		},
		{
			name: "seeded.json names an older pointer and the record is unreadable", reason: "reseeded",
			setup: func(t *testing.T, c *wbCluster, a *wbNode, _ *VM) {
				require.NoError(t, writeUEFISeed(a.dir(), keyFor("older")))
				c.pointers.loadErr = errors.New("nats: no responders")
			},
		},
		{
			name: "the record moved on after this node seeded", reason: "reseeded",
			setup: func(_ *testing.T, c *wbCluster, _ *wbNode, v *VM) {
				moved := &VM{ID: v.ID, UEFIVarStore: &UEFIVarStoreRef{Key: keyFor("restored")}}
				c.pointers.run(nodeA, moved)
			},
		},
		{
			name: "no record of what this node seeded from", reason: "unseeded",
			setup: func(t *testing.T, _ *wbCluster, a *wbNode, _ *VM) {
				require.NoError(t, os.Remove(filepath.Join(a.dir(), uefiSeededFileName)))
			},
		},
		{
			name: "the store record is deleting", reason: "store_not_live",
			setup: func(t *testing.T, c *wbCluster, _ *wbNode, _ *VM) {
				rec, rev, err := c.records.Get(t.Context(), testUEFIInstance)
				require.NoError(t, err)
				rec.State = uefistore.StateDeleting
				_, err = c.records.CompareAndSet(t.Context(), rec, rev)
				require.NoError(t, err)
			},
		},
		{
			name: "a fence stopped it", reason: "fenced",
			setup: func(_ *testing.T, _ *wbCluster, a *wbNode, v *VM) {
				a.m.Inspect(v, func(v *VM) {
					v.Instance.StateReason = &ec2.StateReason{Code: aws.String(fenceStateReasonCode), Message: aws.String("lease moved")}
				})
			},
		},
		{
			name: "recovery left it in error", reason: "error",
			stop: func(_ *testing.T, a *wbNode, v *VM) {
				a.m.MarkRecoveryFailed(v, "relaunch failed")
				a.m.goroutineWg.Wait()
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newWBCluster(t)
			a := c.node(t, nodeA)
			v := a.newInstance(t, region("stale copy"))
			if tt.setup != nil {
				tt.setup(t, c, a, v)
			}
			before := uefiCounter(t, "mulga.vm.uefi_store.write_back_skips", "reason", tt.reason)
			puts := c.objects.putCount()

			if tt.stop != nil {
				tt.stop(t, a, v)
			} else {
				a.stop(t, v)
			}

			assert.Equal(t, puts, c.objects.putCount(), "%s: a skipped copy puts nothing", s7Clause)
			assert.Equal(t, []string{c.seedKey}, c.keys(t), s7Clause)
			var pointer string
			a.m.Inspect(v, func(v *VM) { pointer = v.UEFIVarStore.Key })
			assert.Equal(t, c.seedKey, pointer, "%s: the in-memory pointer, which every record write carries, is unchanged", s7Clause)
			if stopped, _ := c.states.LoadStoppedInstance(v.ID); stopped != nil {
				assert.Equal(t, c.seedKey, stopped.UEFIVarStore.Key, s7Clause)
			}
			if tt.reason != "store_not_live" {
				assert.Nil(t, c.owed(t), "a skip records no obligation")
			}
			assert.Equal(t, before+1, uefiCounter(t, "mulga.vm.uefi_store.write_back_skips", "reason", tt.reason))
		})
	}
}

// The reaper deletes only the object its own obligation put, and only once
// no record points at it: the pointed object and the version it seeded from
// survive every branch.
func TestS7_UEFIWriteBackReaperNeverDeletesThePointedObject(t *testing.T) {
	tests := []struct {
		name        string
		pointer     func(c *wbCluster, owed string) string
		lastNode    string
		wantDeleted bool
	}{
		{name: "the record already points at the obligation's key", pointer: func(_ *wbCluster, owed string) string { return owed }, lastNode: nodeA},
		{name: "an operator restored an older version", pointer: func(*wbCluster, string) string { return keyFor("restored") }, lastNode: nodeA, wantDeleted: true},
		{name: "another node owns the record", pointer: func(c *wbCluster, _ string) string { return c.seedKey }, lastNode: nodeB, wantDeleted: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newWBCluster(t)
			a := c.node(t, nodeA)
			c.put(t, keyFor("restored"), region("restored"))
			v := a.newInstance(t, region("written while kv was down"))
			c.states.stoppedErr = assert.AnError
			a.stop(t, v)
			c.states.stoppedErr = nil
			owed := c.owed(t)
			require.NotNil(t, owed)
			require.True(t, c.exists(t, owed.Key), "the put landed before the record write failed")

			pointer := tt.pointer(c, owed.Key)
			a.m.Delete(v.ID)
			v.LastNode = tt.lastNode
			v.UEFIVarStore = &UEFIVarStoreRef{Key: pointer, Firmware: c.identity}
			require.NoError(t, c.states.WriteStoppedInstance(v.ID, v))

			assert.Equal(t, 1, a.sweep(t))

			assert.Nil(t, c.owed(t))
			assert.True(t, c.exists(t, pointer), "%s: the pointed object is never deleted", s7Clause)
			assert.True(t, c.exists(t, c.seedKey), "%s: the version it seeded from is never deleted", s7Clause)
			assert.Equal(t, !tt.wantDeleted, c.exists(t, owed.Key), "the obligation's own unpointed object is the only one deleted")
			assert.Equal(t, pointer, c.stopped(t).UEFIVarStore.Key, "%s: a dropped obligation never moves the pointer", s7Clause)
		})
	}
}
