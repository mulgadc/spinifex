package systeminstance

import (
	"errors"
	"fmt"
	"regexp"
)

// Kind selects the guest a direct-boot system instance runs. The empty kind
// is the load balancer; every other kind is launched only on its own subject.
type Kind string

// ErrNonLoadBalancerKind refuses a non-load-balancer guest on the legacy path.
var ErrNonLoadBalancerKind = errors.New("sysinstance: guest kind must use its own launch subject")

// volumeIDPattern is the platform's volume ID shape. The guest finds its disk
// by the ID without its dash, which must fit the 20-byte virtio-blk serial.
var volumeIDPattern = regexp.MustCompile(`^vol-[0-9a-f]{17}$`)

// RefuseNonLoadBalancerKind keeps every non-load-balancer guest off the
// legacy launch path, which would boot the load balancer image instead.
func (in *SystemInstanceInput) RefuseNonLoadBalancerKind() error {
	if in.Kind != "" || in.SQSWorker != nil {
		return fmt.Errorf("%w: kind %q", ErrNonLoadBalancerKind, in.Kind)
	}
	return nil
}
