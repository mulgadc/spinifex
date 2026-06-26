package handlers_ecs

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEIPBackend answers the ec2 EIP NATS subjects with canned responses so the
// real natsEIPManager (the ec2-EIP NATS client) can be driven end to end.
type fakeEIPBackend struct {
	mu             sync.Mutex
	associateFails bool
	associatedENI  string
	disassociated  string
	releasedAlloc  string
}

const (
	fakeAllocID  = "eipalloc-test1"
	fakePublicIP = "203.0.113.7"
	fakeAssocID  = "eipassoc-test1"
)

func (b *fakeEIPBackend) register(t *testing.T, nc *nats.Conn) {
	t.Helper()
	reply := func(msg *nats.Msg, out any) { data, _ := json.Marshal(out); _ = msg.Respond(data) }
	handlers := map[string]func(*nats.Msg){
		"ec2.AllocateAddress": func(msg *nats.Msg) {
			reply(msg, &ec2.AllocateAddressOutput{
				AllocationId: aws.String(fakeAllocID), PublicIp: aws.String(fakePublicIP), Domain: aws.String("vpc"),
			})
		},
		"ec2.AssociateAddress": func(msg *nats.Msg) {
			var in ec2.AssociateAddressInput
			_ = json.Unmarshal(msg.Data, &in)
			if b.associateFails {
				_ = msg.Respond(utils.GenerateErrorPayload("InvalidParameterValue"))
				return
			}
			b.mu.Lock()
			b.associatedENI = aws.StringValue(in.NetworkInterfaceId)
			b.mu.Unlock()
			reply(msg, &ec2.AssociateAddressOutput{AssociationId: aws.String(fakeAssocID)})
		},
		"ec2.DescribeAddresses": func(msg *nats.Msg) {
			reply(msg, &ec2.DescribeAddressesOutput{Addresses: []*ec2.Address{
				{AllocationId: aws.String(fakeAllocID), AssociationId: aws.String(fakeAssocID)},
			}})
		},
		"ec2.DisassociateAddress": func(msg *nats.Msg) {
			var in ec2.DisassociateAddressInput
			_ = json.Unmarshal(msg.Data, &in)
			b.mu.Lock()
			b.disassociated = aws.StringValue(in.AssociationId)
			b.mu.Unlock()
			reply(msg, &ec2.DisassociateAddressOutput{})
		},
		"ec2.ReleaseAddress": func(msg *nats.Msg) {
			var in ec2.ReleaseAddressInput
			_ = json.Unmarshal(msg.Data, &in)
			b.mu.Lock()
			b.releasedAlloc = aws.StringValue(in.AllocationId)
			b.mu.Unlock()
			reply(msg, &ec2.ReleaseAddressOutput{})
		},
	}
	for subj, h := range handlers {
		sub, err := nc.Subscribe(subj, h)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
}

func TestNATSEIPManager_AllocateAndAssociate(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	backend := &fakeEIPBackend{}
	backend.register(t, nc)

	pub, alloc, err := newNATSEIPManager(nc).AllocateAndAssociate(testAccountID, "eni-target")
	require.NoError(t, err)
	assert.Equal(t, fakePublicIP, pub)
	assert.Equal(t, fakeAllocID, alloc)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	assert.Equal(t, "eni-target", backend.associatedENI)
}

// A failed AssociateAddress must release the orphaned allocation so nothing leaks.
func TestNATSEIPManager_AssociateFailureReleasesAllocation(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	backend := &fakeEIPBackend{associateFails: true}
	backend.register(t, nc)

	_, _, err := newNATSEIPManager(nc).AllocateAndAssociate(testAccountID, "eni-target")
	require.Error(t, err)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	assert.Equal(t, fakeAllocID, backend.releasedAlloc, "orphaned allocation should be released")
}

func TestNATSEIPManager_ReleaseDisassociatesThenReleases(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	backend := &fakeEIPBackend{}
	backend.register(t, nc)

	require.NoError(t, newNATSEIPManager(nc).Release(testAccountID, fakeAllocID))
	backend.mu.Lock()
	defer backend.mu.Unlock()
	assert.Equal(t, fakeAssocID, backend.disassociated)
	assert.Equal(t, fakeAllocID, backend.releasedAlloc)
}
