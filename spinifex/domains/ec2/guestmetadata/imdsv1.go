package guestmetadata

import (
	"context"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// v1AllowTTL bounds how long a per-instance HttpTokens decision is reused. The
// decision only changes via ModifyInstanceMetadataOptions, so a short TTL costs
// little and keeps the untokened path off the NATS fan-out.
const v1AllowTTL = 30 * time.Second

// imdsV1Allowed reports whether this ENI's instance was launched or modified
// with HttpTokens=optional, which is the only case where an untokened GET is
// served. Defaults closed: an unattached ENI, a lookup failure or a missing
// instance all deny.
func (s *IMDSServiceImpl) imdsV1Allowed(ctx context.Context, eni *eniFacts) bool {
	if eni.instanceID == "" {
		return false
	}
	if allowed, ok := s.v1Allow.get(eni.instanceID); ok {
		return allowed
	}

	inst, err := s.resolver.resolveInstance(ctx, eni)
	if err != nil {
		slog.WarnContext(ctx, "IMDS: HttpTokens lookup failed, denying untokened request",
			"instance_id", eni.instanceID, "err", err)
	}
	allowed := err == nil && inst != nil && inst.httpTokens == ec2.HttpTokensStateOptional

	// Cached even on failure: without that, a guest could replay a failing
	// lookup to keep the fan-out running.
	s.v1Allow.put(eni.instanceID, allowed)
	return allowed
}
