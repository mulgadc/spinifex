package handlers_ec2_instance

import (
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// StatusSelection is a parsed DescribeInstanceStatus request: which instances
// the caller asked about and may see. Parse once per request, then apply to
// each candidate VM.
type StatusSelection struct {
	accountID      string
	instanceIDs    map[string]bool
	filters        map[string][]string
	includedStates map[vm.InstanceState]bool
}

// StatusOptions carries what the projection cannot read off a VM. SystemImpaired
// is node-local memory pressure that only an answering node can report, and AZ
// differs by source: the node's own on the node path, the record's on a cached one.
type StatusOptions struct {
	SystemImpaired bool
	AZ             string
}

// ParseStatusSelection validates the request and resolves which states are in
// scope. Shared so the node path and any cache-backed path cannot drift on what
// a request asked for; the default stays running-only.
func ParseStatusSelection(input *ec2.DescribeInstanceStatusInput, accountID string) (StatusSelection, error) {
	instanceIDs, err := ParseInstanceIDFilter(input.InstanceIds)
	if err != nil {
		return StatusSelection{}, err
	}

	filters, err := filterutil.ParseFilters(input.Filters, DescribeInstanceStatusValidFilters)
	if err != nil {
		slog.Warn("DescribeInstanceStatus: invalid filter", "err", err)
		return StatusSelection{}, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	includedStates := describeInstanceStatusRunningOnly
	if aws.BoolValue(input.IncludeAllInstances) {
		includedStates = describeInstanceStatusAllIncluded
	}

	return StatusSelection{
		accountID:      accountID,
		instanceIDs:    instanceIDs,
		filters:        filters,
		includedStates: includedStates,
	}, nil
}

// Includes reports whether v is part of this request's answer, before any
// status is built. Split out so a caller can test membership without paying
// for the projection.
func (s StatusSelection) Includes(v *vm.VM) bool {
	if v == nil || !IsInstanceVisible(s.accountID, v.AccountID) {
		return false
	}
	if len(s.instanceIDs) > 0 && !s.instanceIDs[v.ID] {
		return false
	}
	return s.includedStates[v.Status]
}

// StatusFor projects v into a status frame, or returns nil when the request
// does not cover it. The status filters run against the built frame, so they
// cannot be applied before projection.
func (s StatusSelection) StatusFor(v *vm.VM, opts StatusOptions) *ec2.InstanceStatus {
	if !s.Includes(v) {
		return nil
	}
	is := buildInstanceStatus(v, opts.SystemImpaired, opts.AZ)
	if len(s.filters) > 0 && !instanceStatusMatchesFilters(v, is, s.filters) {
		return nil
	}
	return is
}
