package networkv1

const (
	// SystemEgressAddSubject asks vpcd to install a /32 reroute plus
	// snat-only NAT for a single system instance. Fire-and-forget. No
	// production publisher remains today; see README.
	SystemEgressAddSubject = "vpc.add-system-egress"

	// SystemEgressDeleteSubject asks vpcd to remove that reroute and NAT.
	// Fire-and-forget.
	SystemEgressDeleteSubject = "vpc.delete-system-egress"
)

// SystemEgressEvent is the payload on SystemEgressAddSubject and
// SystemEgressDeleteSubject.
type SystemEgressEvent struct {
	VpcId      string `json:"vpc_id"`
	SubnetId   string `json:"subnet_id"`
	InstanceIp string `json:"instance_ip"`
	ExternalIp string `json:"external_ip"`
}
