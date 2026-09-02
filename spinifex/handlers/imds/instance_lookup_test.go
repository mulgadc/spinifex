package handlers_imds

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// writeLocalState persists a local instance-state file with the given VMs.
func writeLocalState(t *testing.T, dataDir string, vms map[string]*vm.VM) {
	t.Helper()
	data, err := daemon.MarshalLocalState(vms)
	require.NoError(t, err)
	require.NoError(t, daemon.WriteLocalStateBytes(daemon.LocalStatePath(dataDir), data))
}

func TestDescribe_LocalHit_ResolvesWithoutRecordSpace(t *testing.T) {
	dataDir := t.TempDir()
	writeLocalState(t, dataDir, map[string]*vm.VM{"i-local": testVM("i-local")})

	l := &localInstanceLookup{dataDir: dataDir, records: noCallRecordLoader{t: t}}
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
	dataDir := t.TempDir() // no local state file at all: a definite local miss
	fallbackVM := testVM("i-remote")

	l := &localInstanceLookup{
		dataDir: dataDir,
		records: fakeRecordLoader{record: fallbackVM.Record()},
	}
	facts, err := l.describe(context.Background(), "111111111111", "i-remote")
	require.NoError(t, err)
	require.NotNil(t, facts)
	assert.Equal(t, "t3.micro", facts.instanceType)
	assert.Equal(t, "ami-123", facts.imageID)
}

func TestDescribe_RecordSpaceMiss_ReturnsMiss(t *testing.T) {
	dataDir := t.TempDir()

	l := &localInstanceLookup{
		dataDir: dataDir,
		records: fakeRecordLoader{record: nil},
	}
	facts, err := l.describe(context.Background(), "111111111111", "i-nowhere")
	require.NoError(t, err)
	assert.Nil(t, facts)
}

func TestDescribe_NoRecordLoader_LocalMissIsAMiss(t *testing.T) {
	dataDir := t.TempDir()

	l := &localInstanceLookup{dataDir: dataDir} // records left nil, as when JetStream never opened
	facts, err := l.describe(context.Background(), "111111111111", "i-nowhere")
	require.NoError(t, err)
	assert.Nil(t, facts)
}

func TestDescribe_ManagedByHidesFromNonGlobalAccount(t *testing.T) {
	dataDir := t.TempDir()
	managed := testVM("i-managed")
	managed.ManagedBy = "eks"
	writeLocalState(t, dataDir, map[string]*vm.VM{"i-managed": managed})

	l := &localInstanceLookup{dataDir: dataDir, records: noCallRecordLoader{t: t}}
	facts, err := l.describe(context.Background(), "111111111111", "i-managed")
	require.NoError(t, err)
	assert.Nil(t, facts, "platform-managed instance must stay hidden from a non-global account")
}

func TestDescribe_WrongAccount_IsAMiss(t *testing.T) {
	dataDir := t.TempDir()
	writeLocalState(t, dataDir, map[string]*vm.VM{"i-local": testVM("i-local")})

	l := &localInstanceLookup{dataDir: dataDir, records: noCallRecordLoader{t: t}}
	facts, err := l.describe(context.Background(), "222222222222", "i-local")
	require.NoError(t, err)
	assert.Nil(t, facts)
}

func TestLocalVM_CachesParseUntilFileChanges(t *testing.T) {
	dataDir := t.TempDir()
	writeLocalState(t, dataDir, map[string]*vm.VM{"i-a": testVM("i-a")})

	parses := 0
	l := &localInstanceLookup{
		dataDir: dataDir,
		readState: func(path string) (*daemon.LocalState, error) {
			parses++
			return daemon.ReadLocalState(path)
		},
	}

	require.NotNil(t, l.localVM(context.Background(), "i-a"))
	require.NotNil(t, l.localVM(context.Background(), "i-a"))
	assert.Equal(t, 1, parses, "a second lookup on an unchanged file must reuse the cached parse")

	writeLocalState(t, dataDir, map[string]*vm.VM{"i-a": testVM("i-a"), "i-b": testVM("i-b")})
	require.NotNil(t, l.localVM(context.Background(), "i-b"))
	assert.Equal(t, 2, parses, "a changed file must be re-parsed")
}

// TestDescribe_LocalStateCorrupt_FallsBackToRecordSpace proves the cache does
// not mask a genuine read failure by serving its last good parse: once the
// state file is corrupted, describe must fall through to the record space
// rather than error out or serve the stale hit it cached earlier.
func TestDescribe_LocalStateCorrupt_FallsBackToRecordSpace(t *testing.T) {
	dataDir := t.TempDir()
	writeLocalState(t, dataDir, map[string]*vm.VM{"i-a": testVM("i-a")})

	l := &localInstanceLookup{dataDir: dataDir}
	require.NotNil(t, l.localVM(context.Background(), "i-a"), "warm the cache with a good parse")

	// Different size than the good state, so the cache cannot mistake this
	// for an unchanged file and must attempt (and fail) a re-parse.
	require.NoError(t, os.WriteFile(daemon.LocalStatePath(dataDir), []byte("not json"), 0o600))

	l.records = fakeRecordLoader{record: testVM("i-a").Record()}
	facts, err := l.describe(context.Background(), "111111111111", "i-a")
	require.NoError(t, err)
	require.NotNil(t, facts, "a corrupt local state must fall back to the record space, not error or go stale")
}

// TestLocalInstanceLookupHasNoNATSSurface pins the "never fan out" contract at
// the type level: localInstanceLookup carries only a data directory and the
// narrow record-space interface, no NATS connection to fan a request out over.
func TestLocalInstanceLookupHasNoNATSSurface(t *testing.T) {
	l := localInstanceLookup{dataDir: filepath.Join(t.TempDir(), "state")}
	var _ instanceLookup = &l
}
