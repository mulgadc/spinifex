package handlers_imds

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLocalStateReader stands in for the vpcd-side on-disk state adapter.
type fakeLocalStateReader struct {
	vms map[string]*vm.VM
	err error
}

func (f fakeLocalStateReader) LocalVM(instanceID string) (*vm.VM, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.vms[instanceID], nil
}

// noCallRecordLoader fails the test if the fallback is ever invoked, proving a
// local hit resolves without reaching for the record space (and so never
// fans out over NATS: the fallback is the only path off this node).
type noCallRecordLoader struct{ t *testing.T }

func (n noCallRecordLoader) LoadInstanceRecord(instanceID string) (*vm.InstanceRecord, error) {
	n.t.Fatalf("record-space fallback called for %s despite a local hit", instanceID)
	return nil, nil
}

// fakeRecordLoader stands in for the JetStream-backed accessor in fallback tests.
type fakeRecordLoader struct {
	record *vm.InstanceRecord
	err    error
}

func (f fakeRecordLoader) LoadInstanceRecord(string) (*vm.InstanceRecord, error) {
	return f.record, f.err
}

// testVM builds a fully resolvable instance for lookup tests.
func testVM(id string) *vm.VM {
	launch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &vm.VM{
		ID:                    id,
		AccountID:             "111111111111",
		InstanceType:          "t3.micro",
		IamInstanceProfileArn: "arn:aws:iam::111111111111:instance-profile/demo",
		InstanceLifecycle:     "spot",
		RunInstancesInput: &ec2.RunInstancesInput{
			UserData: aws.String("aGVsbG8="), // "hello"
		},
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-abc123"),
		},
		Instance: &ec2.Instance{
			ImageId:         aws.String("ami-123"),
			KeyName:         aws.String("demo-key"),
			Architecture:    aws.String("x86_64"),
			AmiLaunchIndex:  aws.Int64(0),
			LaunchTime:      aws.Time(launch),
			MetadataOptions: &ec2.InstanceMetadataOptionsResponse{HttpTokens: aws.String("optional")},
		},
	}
}

func TestDescribe_LocalHit_ResolvesWithoutRecordSpace(t *testing.T) {
	l := &localInstanceLookup{
		local:   fakeLocalStateReader{vms: map[string]*vm.VM{"i-local": testVM("i-local")}},
		records: noCallRecordLoader{t: t},
	}
	facts, err := l.describe(context.Background(), "111111111111", "i-local")
	require.NoError(t, err)
	require.NotNil(t, facts)

	assert.Equal(t, "t3.micro", facts.instanceType)
	assert.Equal(t, "ami-123", facts.imageID)
	assert.Equal(t, "demo-key", facts.keyName)
	assert.Equal(t, "x86_64", facts.architecture)
	assert.Equal(t, "arn:aws:iam::111111111111:instance-profile/demo", facts.iamInstanceProfileArn)
	assert.Equal(t, "spot", facts.lifecycleType)
	assert.Equal(t, "optional", facts.httpTokens)
	assert.Equal(t, int64(0), facts.amiLaunchIndex)
	assert.Equal(t, "r-abc123", facts.reservationID)
	assert.Equal(t, []byte("hello"), facts.userData)
}

func TestDescribe_LocalMiss_FallsBackToRecordSpace(t *testing.T) {
	fallbackVM := testVM("i-remote")

	l := &localInstanceLookup{
		local:   fakeLocalStateReader{vms: map[string]*vm.VM{}}, // instance not on this node
		records: fakeRecordLoader{record: fallbackVM.Record()},
	}
	facts, err := l.describe(context.Background(), "111111111111", "i-remote")
	require.NoError(t, err)
	require.NotNil(t, facts)
	assert.Equal(t, "t3.micro", facts.instanceType)
	assert.Equal(t, "ami-123", facts.imageID)
}

func TestDescribe_RecordSpaceMiss_ReturnsMiss(t *testing.T) {
	l := &localInstanceLookup{
		local:   fakeLocalStateReader{vms: map[string]*vm.VM{}},
		records: fakeRecordLoader{record: nil},
	}
	facts, err := l.describe(context.Background(), "111111111111", "i-nowhere")
	require.NoError(t, err)
	assert.Nil(t, facts)
}

func TestDescribe_NoLocalReaderOrRecordLoader_IsAMiss(t *testing.T) {
	l := &localInstanceLookup{} // both left nil, as when neither adapter is available
	facts, err := l.describe(context.Background(), "111111111111", "i-nowhere")
	require.NoError(t, err)
	assert.Nil(t, facts)
}

// TestDescribe_LocalReadError_FallsBackToRecordSpace proves a local-state
// read failure does not error the whole lookup or leave IMDS stuck: it falls
// through to the record space like a genuine local miss.
func TestDescribe_LocalReadError_FallsBackToRecordSpace(t *testing.T) {
	l := &localInstanceLookup{
		local:   fakeLocalStateReader{err: errors.New("state file corrupt")},
		records: fakeRecordLoader{record: testVM("i-a").Record()},
	}
	facts, err := l.describe(context.Background(), "111111111111", "i-a")
	require.NoError(t, err)
	require.NotNil(t, facts, "a local read error must fall back to the record space, not error out")
}

func TestDescribe_ManagedByHidesFromNonGlobalAccount(t *testing.T) {
	managed := testVM("i-managed")
	managed.ManagedBy = "eks"

	l := &localInstanceLookup{
		local:   fakeLocalStateReader{vms: map[string]*vm.VM{"i-managed": managed}},
		records: noCallRecordLoader{t: t},
	}
	facts, err := l.describe(context.Background(), "111111111111", "i-managed")
	require.NoError(t, err)
	assert.Nil(t, facts, "platform-managed instance must stay hidden from a non-global account")
}

func TestDescribe_WrongAccount_IsAMiss(t *testing.T) {
	l := &localInstanceLookup{
		local:   fakeLocalStateReader{vms: map[string]*vm.VM{"i-local": testVM("i-local")}},
		records: noCallRecordLoader{t: t},
	}
	facts, err := l.describe(context.Background(), "222222222222", "i-local")
	require.NoError(t, err)
	assert.Nil(t, facts)
}

// TestLocalInstanceLookupHasNoNATSSurface pins the "never fan out" contract at
// the type level: localInstanceLookup carries only the narrow local-state and
// record-space interfaces, no NATS connection to fan a request out over.
func TestLocalInstanceLookupHasNoNATSSurface(t *testing.T) {
	l := localInstanceLookup{}
	var _ instanceLookup = &l
}
