package projection

import (
	"context"
	"regexp"

	"github.com/aws/aws-sdk-go/service/ec2"
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
	// State is the EC2 instance state name, such as "running" or "stopped".
	State string
	// PrimaryInterfaceID is empty when the record has no consistent primary
	// interface; VpcID and SubnetID are that interface's placement.
	PrimaryInterfaceID string
	VpcID              string
	SubnetID           string
	// Deleting is true once a terminate is decided, even while still running.
	Deleting bool
}

// Running reports whether the instance's observed state is running.
func (n *NamedInstance) Running() bool {
	return n.State == ec2.InstanceStateNameRunning
}

// NamedInstanceLookup reads one named instance as a caller in accountID sees
// it. A missing, hidden or malformed ID is nil with no error; a failed read
// or a record under another identity is an error.
type NamedInstanceLookup interface {
	NamedInstance(ctx context.Context, accountID, instanceID string) (*NamedInstance, error)
}
