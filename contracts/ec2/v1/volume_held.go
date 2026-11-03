package ec2v1

import "errors"

// VolumeHeldElsewhereText is the message viperblockd gives a lease refusal. It
// is wording for the operator; callers classify the refusal by its code.
const VolumeHeldElsewhereText = "volume is leased by another owner"

// ErrVolumeHeldElsewhere marks a start refused because another node still
// holds one of the instance's volumes. The owning node sets it from the EBS
// provider's held-elsewhere code, and a start reply carries it across NATS as
// the X-Volume-Held-Elsewhere header rather than as message text.
var ErrVolumeHeldElsewhere = errors.New("a volume is leased by another node")
