package handlers_ec2_instance

import (
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// InstanceProjection carries the caller-scoped values ProjectInstance needs.
// All fields are plain data so the projection stays pure and table-testable.
type InstanceProjection struct {
	Region            string
	DNSBaseDomain     string
	DNSInternalDomain string
	AZ                string

	// IncludeRuntimeNetwork projects the fields that only hold while an instance
	// runs: the public IP, its derived DNS names, and the consumed capacity
	// reservation. AWS releases all three when an instance stops, so the
	// stopped/terminated path leaves IncludeRuntimeNetwork false and they stay
	// unset. Placement and Spot lineage survive a stop and are projected either
	// way.
	IncludeRuntimeNetwork bool

	// FallbackStateCode/Name label the state when vm.EC2APIState has no mapping
	// for the instance's internal status.
	FallbackStateCode int64
	FallbackStateName string
}

// ProjectInstance returns the API-shaped copy of v.Instance with every
// vm.VM-sourced field projected onto it. It is pure: it reads only v and cfg and
// mutates a fresh copy, so both live describe paths share one field set and
// cannot drift apart. v.Instance must be non-nil.
//
// stateMapped is false when v.Status has no EC2 equivalent; the caller's
// fallback code/name is applied either way, and the flag lets the running path
// log the gap without the stopped path having to.
func ProjectInstance(v *vm.VM, cfg InstanceProjection) (inst *ec2.Instance, stateMapped bool) {
	instanceCopy := *v.Instance
	instanceCopy.State = &ec2.InstanceState{}

	// Public IP and its derived DNS names exist only while the instance runs; a
	// stopped instance has released the public IP, so the KV path leaves them
	// unset. Mirrors the records the control-plane writer publishes to northstar.
	if cfg.IncludeRuntimeNetwork {
		if v.PublicIP != "" && instanceCopy.PublicIpAddress == nil {
			instanceCopy.PublicIpAddress = aws.String(v.PublicIP)
		}

		publicDNS, privateDNS := handlers_dns.EC2DNSNames(
			cfg.Region, cfg.DNSBaseDomain, cfg.DNSInternalDomain,
			aws.StringValue(instanceCopy.PublicIpAddress), aws.StringValue(instanceCopy.PrivateIpAddress),
		)
		if publicDNS != "" {
			instanceCopy.PublicDnsName = aws.String(publicDNS)
		}
		if privateDNS != "" {
			instanceCopy.PrivateDnsName = aws.String(privateDNS)
		}
	}

	// Map internal status to AWS state, projecting Spinifex-only states
	// (e.g. error -> stopped) so SDK/UI clients see a valid label. An unmapped
	// status falls back to the caller's code/name and reports stateMapped false.
	if info, ok := vm.EC2APIState(v.Status); ok {
		instanceCopy.State.SetCode(info.Code)
		instanceCopy.State.SetName(info.Name)
		stateMapped = true
	} else {
		instanceCopy.State.SetCode(cfg.FallbackStateCode)
		instanceCopy.State.SetName(cfg.FallbackStateName)
	}

	// Project IamInstanceProfile from vm.VM (single source of truth across the
	// Associate/Disassociate/Replace lifecycle). Id is left nil — the gateway
	// resolves it via IAMService post-aggregation since daemons have no IAM
	// access. Empty Arn clears any stale reference left on the stored
	// instance.Instance (e.g. after Disassociate or auto-clear on terminate).
	if v.IamInstanceProfileArn != "" {
		instanceCopy.IamInstanceProfile = &ec2.IamInstanceProfile{
			Arn: aws.String(v.IamInstanceProfileArn),
		}
	} else {
		instanceCopy.IamInstanceProfile = nil
	}

	// Placement survives a stop in AWS, so project it regardless of runtime state
	// whenever the instance belongs to a placement group.
	if v.PlacementGroupName != "" {
		instanceCopy.Placement = &ec2.Placement{
			GroupName:        aws.String(v.PlacementGroupName),
			AvailabilityZone: aws.String(cfg.AZ),
		}
	}

	// Echo the consumed capacity reservation so targeted-launch Terraform
	// converges — without it the instance reports no reservation and the plan
	// never settles. Gated to running instances: AWS releases the reservation
	// when an instance stops, so a stopped instance must not echo it.
	if cfg.IncludeRuntimeNetwork && v.CapacityReservationId != "" {
		instanceCopy.CapacityReservationId = aws.String(v.CapacityReservationId)
		instanceCopy.CapacityReservationSpecification = &ec2.CapacityReservationSpecificationResponse{
			CapacityReservationPreference: aws.String(ec2.CapacityReservationPreferenceOpen),
			CapacityReservationTarget: &ec2.CapacityReservationTargetResponse{
				CapacityReservationId: aws.String(v.CapacityReservationId),
			},
		}
	}

	// Spot lineage stamped by the post-launch write-back survives a stop, so
	// project it regardless of runtime state. Both empty for on-demand, so the
	// fields stay absent there.
	if v.InstanceLifecycle != "" {
		instanceCopy.InstanceLifecycle = aws.String(v.InstanceLifecycle)
	}
	if v.SpotInstanceRequestId != "" {
		instanceCopy.SpotInstanceRequestId = aws.String(v.SpotInstanceRequestId)
	}

	return &instanceCopy, stateMapped
}

// ParseInstanceIDFilter builds the set of instance IDs to filter on, rejecting
// any ID without the "i-" prefix (including the empty string) as malformed. Nil
// entries carry no constraint and are skipped. Empty input yields an empty set,
// which callers treat as "no filter". It is pure so both describe paths validate
// identically.
func ParseInstanceIDFilter(ids []*string) (map[string]bool, error) {
	filter := make(map[string]bool)
	for _, id := range ids {
		if id == nil {
			continue
		}
		if !strings.HasPrefix(*id, "i-") {
			return nil, errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
		}
		filter[*id] = true
	}
	return filter, nil
}
