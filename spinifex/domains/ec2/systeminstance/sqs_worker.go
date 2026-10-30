package systeminstance

import (
	"errors"
	"fmt"

	"github.com/nats-io/nkeys"
)

// KindSQSWorker is the shared SQS worker guest. It holds the store's private
// key on its own data volume and reaches the gateway over its mgmt NIC.
const KindSQSWorker Kind = "sqs-worker"

// Store status values the worker guest reads at boot. A new store is never
// keyed by the guest from this value alone; it refuses to boot on it.
const (
	SQSWorkerStoreNew      = "new"
	SQSWorkerStoreExisting = "existing"
)

// SQSWorkerInput is what an SQS worker guest needs beyond its network: its
// existing encrypted data volume and the first-store decision read at launch.
// StoreNKey is the store's recorded public key; nothing here is ever a seed.
type SQSWorkerInput struct {
	DataVolumeID string `json:"data_volume_id"`
	StoreStatus  string `json:"store_status"`
	StoreNKey    string `json:"store_nkey,omitempty"`
}

// ErrInvalidSQSWorkerInput is returned for a malformed SQS worker launch.
var ErrInvalidSQSWorkerInput = errors.New("sysinstance: invalid SQS worker launch")

// ValidateSQSWorker checks an SQS worker launch's shape. The worker must have
// an instance profile and a mgmt NIC placeholder: it has no other path to the
// gateway and no static credential.
func (in *SystemInstanceInput) ValidateSQSWorker() error {
	switch {
	case in.Kind != KindSQSWorker:
		return fmt.Errorf("%w: kind %q is not %q", ErrInvalidSQSWorkerInput, in.Kind, KindSQSWorker)
	case in.BootMode != "" && in.BootMode != BootDirect:
		return fmt.Errorf("%w: boot mode %q, the worker boots direct only", ErrInvalidSQSWorkerInput, in.BootMode)
	case in.SQSWorker == nil:
		return fmt.Errorf("%w: no data volume or store decision", ErrInvalidSQSWorkerInput)
	case in.SQSStore != nil:
		return fmt.Errorf("%w: SQS store settings on an SQS worker launch", ErrInvalidSQSWorkerInput)
	case in.IamInstanceProfileArn == "":
		return fmt.Errorf("%w: no instance profile", ErrInvalidSQSWorkerInput)
	case in.LBAgentEnv != "" || in.CACert != "" || in.Scheme != "":
		// The daemon supplies the CA from its own trust store, never the caller.
		return fmt.Errorf("%w: load balancer settings on an SQS worker launch", ErrInvalidSQSWorkerInput)
	case len(in.NICs) != 2 || !in.NICs[0].IsDefault || in.NICs[1].IsDefault || in.NICs[1].MAC != "" || in.NICs[1].CIDR != "":
		return fmt.Errorf("%w: want a default VPC NIC and an unallocated mgmt NIC", ErrInvalidSQSWorkerInput)
	}
	return in.SQSWorker.Validate()
}

// Validate checks the worker's volume and store decision. It reads nothing.
func (w *SQSWorkerInput) Validate() error {
	if !volumeIDPattern.MatchString(w.DataVolumeID) {
		return fmt.Errorf("%w: data volume ID %q is malformed", ErrInvalidSQSWorkerInput, w.DataVolumeID)
	}
	switch w.StoreStatus {
	case SQSWorkerStoreNew:
		if w.StoreNKey != "" {
			return fmt.Errorf("%w: a new store has no recorded key", ErrInvalidSQSWorkerInput)
		}
	case SQSWorkerStoreExisting:
		if !nkeys.IsValidPublicUserKey(w.StoreNKey) {
			// Not echoed: a caller that sent a seed must not have it logged.
			return fmt.Errorf("%w: store key is not a NATS user public key", ErrInvalidSQSWorkerInput)
		}
	default:
		return fmt.Errorf("%w: store status is neither %q nor %q", ErrInvalidSQSWorkerInput, SQSWorkerStoreNew, SQSWorkerStoreExisting)
	}
	return nil
}
