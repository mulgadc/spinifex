package instance

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go/jetstream"
)

// The regional instance record space. The daemon owns these names; a test
// pins these copies to them.
const (
	instanceStateBucket  = "spinifex-instance-state"
	instanceRecordPrefix = "i."
)

// instanceIDPattern matches the instance ID forms AWS and this stack issue.
var instanceIDPattern = regexp.MustCompile(`^i-([0-9a-f]{8}|[0-9a-f]{17})$`)

// ValidInstanceID reports whether id has the form of an instance ID.
func ValidInstanceID(id string) bool {
	return instanceIDPattern.MatchString(id)
}

// NamedInstance is what a caller may learn about one instance it names: its
// owner, observed state and primary interface placement. It carries no record.
type NamedInstance struct {
	InstanceID string
	AccountID  string
	State      vm.InstanceState
	// PrimaryInterfaceID is empty when the record has no consistent primary
	// interface; VpcID and SubnetID are that interface's placement.
	PrimaryInterfaceID string
	VpcID              string
	SubnetID           string
	// Deleting is true once a terminate is decided, even while still running.
	Deleting bool
}

// NamedInstanceLookup reads one named instance as a caller in accountID sees
// it. A missing or hidden instance is nil with no error.
type NamedInstanceLookup interface {
	NamedInstance(ctx context.Context, accountID, instanceID string) (*NamedInstance, error)
}

// NamedInstanceReader point-reads the live instance record space under the
// caller's context. It never lists records or reads the terminated space.
type NamedInstanceReader struct {
	records leaderRecords
}

// leaderRecords is the only read the reader makes: one record from the stream leader.
type leaderRecords interface {
	LeaderGet(ctx context.Context, key string) (*vm.InstanceRecord, uint64, error)
}

var _ leaderRecords = (*kvstore.Store[vm.InstanceRecord])(nil)

var _ NamedInstanceLookup = (*NamedInstanceReader)(nil)

// NewNamedInstanceReader attaches to the existing record space on first read
// and never creates it.
func NewNamedInstanceReader(js jetstream.JetStream) *NamedInstanceReader {
	return &NamedInstanceReader{records: kvstore.New[vm.InstanceRecord](js, kvstore.Config{Name: instanceStateBucket, History: 1, AttachOnly: true})}
}

// NamedInstance implements NamedInstanceLookup. The stream leader answers, so
// a lagging replica's older state is never seen. A malformed ID reads nothing,
// and a failed read or a record under another identity is an error.
func (r *NamedInstanceReader) NamedInstance(ctx context.Context, accountID, instanceID string) (*NamedInstance, error) {
	if !ValidInstanceID(instanceID) {
		return nil, nil
	}
	record, _, err := r.records.LeaderGet(ctx, instanceRecordPrefix+instanceID)
	if errors.Is(err, kvstore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read instance %s: %w", instanceID, err)
	}
	if record.Metadata.Name != instanceID {
		return nil, fmt.Errorf("instance record %s names %q", instanceID, record.Metadata.Name)
	}
	return ProjectNamedInstance(accountID, instanceID, vm.VMFromRecord(record)), nil
}

// ProjectNamedInstance applies the caller visibility rule to v and projects
// the named facts. A hidden instance is nil.
func ProjectNamedInstance(accountID, instanceID string, v *vm.VM) *NamedInstance {
	if !IsInstanceVisibleToCaller(accountID, v) {
		return nil
	}
	out := &NamedInstance{
		InstanceID: instanceID,
		AccountID:  v.AccountID,
		State:      v.Status,
		Deleting:   v.DeletionTimestamp != nil,
	}
	if v.Instance == nil || v.ENIId == "" {
		return out
	}
	for _, nic := range v.Instance.NetworkInterfaces {
		if nic == nil || nic.Attachment == nil || nic.Attachment.DeviceIndex == nil || *nic.Attachment.DeviceIndex != 0 {
			continue
		}
		if aws.StringValue(nic.NetworkInterfaceId) == v.ENIId && aws.StringValue(nic.VpcId) != "" {
			out.PrimaryInterfaceID = v.ENIId
			out.VpcID = aws.StringValue(nic.VpcId)
			out.SubnetID = aws.StringValue(nic.SubnetId)
		}
		break
	}
	return out
}
