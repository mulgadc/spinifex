package systeminstance

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/nats-io/nkeys"
)

// Kind selects the guest a direct-boot system instance runs. The empty kind
// is the load balancer; every other kind is launched only on its own subject.
type Kind string

// KindSQSStore is the dedicated SQS store guest on a persistent data volume.
const KindSQSStore Kind = "sqs-store"

// SQSStoreLaunchSubjectRoot is the subject root for SQS store launches. It is
// versioned and separate from system.LaunchInstance so a daemon that predates
// it has no subscriber and the launch fails instead of booting another guest.
const SQSStoreLaunchSubjectRoot = "system.LaunchSQSStore.v1"

// SQSStoreLaunchSubject is the queue subject for an instance type; with a node
// ID it is that node's targeted subject.
func SQSStoreLaunchSubject(instanceType, nodeID string) string {
	if nodeID == "" {
		return fmt.Sprintf("%s.%s", SQSStoreLaunchSubjectRoot, instanceType)
	}
	return fmt.Sprintf("%s.%s.%s", SQSStoreLaunchSubjectRoot, instanceType, nodeID)
}

// SQSStoreInput is what an SQS store guest needs beyond its network: an
// existing, already-formatted system-account volume and the one NATS user
// public key the store accepts. It never carries a private key or seed.
type SQSStoreInput struct {
	DataVolumeID string `json:"data_volume_id"`
	ClientNKey   string `json:"client_nkey"`
	Region       string `json:"region"`
}

// ErrInvalidSQSStoreInput is returned for a malformed SQS store launch.
var ErrInvalidSQSStoreInput = errors.New("sysinstance: invalid SQS store launch")

// volumeIDPattern is the platform's volume ID shape. The guest finds its disk
// by the ID without its dash, which must fit the 20-byte virtio-blk serial.
var volumeIDPattern = regexp.MustCompile(`^vol-[0-9a-f]{17}$`)

// ValidateSQSStore checks an SQS store launch's shape. It reads nothing, so a
// valid result says only that the launch is well formed.
func (in *SystemInstanceInput) ValidateSQSStore() error {
	switch {
	case in.Kind != KindSQSStore:
		return fmt.Errorf("%w: kind %q is not %q", ErrInvalidSQSStoreInput, in.Kind, KindSQSStore)
	case in.BootMode != "" && in.BootMode != BootDirect:
		return fmt.Errorf("%w: boot mode %q, the store boots direct only", ErrInvalidSQSStoreInput, in.BootMode)
	case in.SQSStore == nil:
		return fmt.Errorf("%w: no data volume or client key", ErrInvalidSQSStoreInput)
	case in.SQSWorker != nil:
		return fmt.Errorf("%w: SQS worker settings on an SQS store launch", ErrInvalidSQSStoreInput)
	case in.LBAgentEnv != "" || in.CACert != "" || in.Scheme != "":
		// The daemon supplies the CA from its own trust store, never the caller.
		return fmt.Errorf("%w: load balancer settings on an SQS store launch", ErrInvalidSQSStoreInput)
	case in.IamInstanceProfileArn == "":
		// The store records its server key pin with this role's credentials.
		return fmt.Errorf("%w: no instance profile", ErrInvalidSQSStoreInput)
	case len(in.NICs) != 2 || !in.NICs[0].IsDefault || in.NICs[1].IsDefault || in.NICs[1].MAC != "" || in.NICs[1].CIDR != "":
		return fmt.Errorf("%w: want a default VPC NIC and an unallocated mgmt NIC", ErrInvalidSQSStoreInput)
	}
	return in.SQSStore.Validate()
}

// Validate checks the store's volume, client key and Region. It reads nothing.
func (s *SQSStoreInput) Validate() error {
	switch {
	case !volumeIDPattern.MatchString(s.DataVolumeID):
		return fmt.Errorf("%w: data volume ID %q is malformed", ErrInvalidSQSStoreInput, s.DataVolumeID)
	case !nkeys.IsValidPublicUserKey(s.ClientNKey):
		// The value is not echoed: a caller that sent a seed here must not
		// have it written to a log.
		return fmt.Errorf("%w: client key is not a NATS user public key", ErrInvalidSQSStoreInput)
	case !regionPattern.MatchString(s.Region):
		return fmt.Errorf("%w: Region is malformed", ErrInvalidSQSStoreInput)
	}
	return nil
}

// RefuseNonLoadBalancerKind keeps every non-load-balancer guest off the
// legacy launch path, which would boot the load balancer image instead.
func (in *SystemInstanceInput) RefuseNonLoadBalancerKind() error {
	if in.Kind != "" || in.SQSStore != nil || in.SQSWorker != nil {
		return fmt.Errorf("%w: kind %q must use its own launch subject", ErrInvalidSQSStoreInput, in.Kind)
	}
	return nil
}
