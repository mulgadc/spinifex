package instance

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	instancesvc "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	filterutil "github.com/mulgadc/spinifex/spinifex/foundation/aws/filters"
	"github.com/nats-io/nats.go"
)

// DescribeInstanceTypes fans out to all nodes and aggregates instance type info.
// nodeIDs is the configured node set: a named type is reported as not existing
// only when every one of those nodes answered, since any of them may host it.
func DescribeInstanceTypes(ctx context.Context, input *ec2.DescribeInstanceTypesInput, natsConn *nats.Conn, expectedNodes int, nodeIDs []string, accountID string) (*ec2.DescribeInstanceTypesOutput, error) {
	jsonData, err := json.Marshal(input)
	if err != nil {
		slog.ErrorContext(ctx, "DescribeInstanceTypes: Failed to marshal input", "err", err)
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	// Only a request naming types can assert absence, so only it waits on
	// every configured node by identity.
	gatherOpts := natsmsg.GatherOpts{Timeout: 3 * time.Second, AccountID: accountID}
	proveAbsence := len(input.InstanceTypes) > 0 && len(nodeIDs) > 0
	if proveAbsence {
		gatherOpts.ExpectedResponders = len(nodeIDs)
	} else {
		gatherOpts.ExpectedNodes = expectedNodes
	}

	frames, sum, err := natsmsg.Gather(ctx, natsConn, "ec2.DescribeInstanceTypes", jsonData, gatherOpts)
	if err != nil {
		return nil, err
	}

	var allInstanceTypes []*ec2.InstanceTypeInfo
	availability := make(map[string]int64)
	answered := make(map[string]bool, len(frames))
	usable := 0
	for _, frame := range frames {
		var nodeOutput instancesvc.DescribeInstanceTypesNodeOutput
		if json.Unmarshal(frame.Data, &nodeOutput) == nil {
			allInstanceTypes = append(allInstanceTypes, nodeOutput.InstanceTypes...)
			for name, count := range nodeOutput.Availability {
				availability[name] += count
			}
			usable++
			if frame.NodeID != "" {
				answered[frame.NodeID] = true
			}
		}
	}

	// capacity=true reports cluster-wide free slots per type.
	showCapacity := false
	for _, f := range input.Filters {
		if f.Name != nil && *f.Name == "capacity" {
			for _, v := range f.Values {
				if v != nil && *v == "true" {
					showCapacity = true
					break
				}
			}
		}
	}

	requestedTypes := make(map[string]bool)
	for _, it := range input.InstanceTypes {
		if it != nil {
			requestedTypes[*it] = true
		}
	}

	var finalInstanceTypes []*ec2.InstanceTypeInfo
	if showCapacity {
		// No node answered, so nothing is known about capacity. Reporting an
		// empty list here reads as "no capacity anywhere", which is what made
		// the console refuse every launch.
		if usable == 0 {
			return nil, awserrors.WithRetryAfter(
				awserrors.Errorf(awserrors.ErrorServiceUnavailable,
					"cannot determine cluster capacity: no node answered the describe fan-out"),
				describeIncompleteRetryAfter,
			)
		}
		finalInstanceTypes = expandAvailability(allInstanceTypes, availability)
	} else {
		seen := make(map[string]bool)
		for _, it := range allInstanceTypes {
			if it != nil && it.InstanceType != nil {
				if !seen[*it.InstanceType] {
					seen[*it.InstanceType] = true
					finalInstanceTypes = append(finalInstanceTypes, it)
				}
			}
		}
	}

	if len(requestedTypes) > 0 {
		var filtered []*ec2.InstanceTypeInfo
		for _, it := range finalInstanceTypes {
			if it != nil && it.InstanceType != nil && requestedTypes[*it.InstanceType] {
				filtered = append(filtered, it)
			}
		}
		finalInstanceTypes = filtered

		if missing := missingInstanceTypes(finalInstanceTypes, input.InstanceTypes); len(missing) > 0 {
			if !proveAbsence || !everyNodeAnswered(nodeIDs, answered, sum) {
				return nil, awserrors.WithRetryAfter(
					awserrors.Errorf(awserrors.ErrorServiceUnavailable,
						"cannot confirm instance type(s) %s: describe fan-out did not complete", strings.Join(missing, ", ")),
					describeIncompleteRetryAfter,
				)
			}
			return nil, awserrors.Errorf(awserrors.ErrorInvalidInstanceType,
				"The following supplied instance types do not exist: [%s]", strings.Join(missing, ", "))
		}
	}

	if values := instanceTypeFilterValues(input.Filters); values != nil {
		var filtered []*ec2.InstanceTypeInfo
		for _, it := range finalInstanceTypes {
			if it != nil && it.InstanceType != nil && filterutil.MatchesAny(values, *it.InstanceType) {
				filtered = append(filtered, it)
			}
		}
		finalInstanceTypes = filtered
	}

	// Fan-out replies arrive in any order; a page boundary needs a stable one.
	sort.SliceStable(finalInstanceTypes, func(i, j int) bool {
		return aws.StringValue(finalInstanceTypes[i].InstanceType) < aws.StringValue(finalInstanceTypes[j].InstanceType)
	})

	output := &ec2.DescribeInstanceTypesOutput{InstanceTypes: finalInstanceTypes}

	// Unpaged unless asked: the capacity listing the UI reads has one entry per
	// free slot and does not follow NextToken.
	if input.MaxResults != nil || aws.StringValue(input.NextToken) != "" {
		page, nextToken, err := pageByOffset(finalInstanceTypes, input.NextToken, input.MaxResults)
		if err != nil {
			return nil, err
		}
		output.InstanceTypes = page
		output.NextToken = nextToken
	}

	slog.InfoContext(ctx, "DescribeInstanceTypes: Aggregated response", "total_instance_types", len(finalInstanceTypes), "show_capacity", showCapacity)
	return output, nil
}

// expandAvailability returns one entry per free cluster-wide slot for each
// type, deduplicating the per-node descriptions first.
//
// The client SDK drops response fields it does not model, so a count cannot
// reach the console any other way. Nodes send counts, and only this last hop
// repeats the entry.
func expandAvailability(types []*ec2.InstanceTypeInfo, availability map[string]int64) []*ec2.InstanceTypeInfo {
	byName := make(map[string]*ec2.InstanceTypeInfo, len(types))
	order := make([]string, 0, len(types))
	for _, it := range types {
		if it == nil || it.InstanceType == nil {
			continue
		}
		if _, seen := byName[*it.InstanceType]; !seen {
			byName[*it.InstanceType] = it
			order = append(order, *it.InstanceType)
		}
	}

	expanded := make([]*ec2.InstanceTypeInfo, 0, len(order))
	for _, name := range order {
		for range availability[name] {
			expanded = append(expanded, byName[name])
		}
	}
	return expanded
}

// missingInstanceTypes returns the requested types absent from found, in the
// caller's order and without repeats.
func missingInstanceTypes(found []*ec2.InstanceTypeInfo, requested []*string) []string {
	present := make(map[string]bool, len(found))
	for _, it := range found {
		present[aws.StringValue(it.InstanceType)] = true
	}
	var missing []string
	for _, r := range requested {
		if r == nil || present[*r] {
			continue
		}
		present[*r] = true
		missing = append(missing, *r)
	}
	return missing
}

// everyNodeAnswered reports whether each configured node sent a usable reply
// and none sent an error, so a type absent from the aggregate is absent.
func everyNodeAnswered(nodeIDs []string, answered map[string]bool, sum natsmsg.Summary) bool {
	for _, id := range nodeIDs {
		if !answered[id] || sum.ErrorResponders[id] {
			return false
		}
	}
	return true
}

// instanceTypeFilterValues returns the values of every instance-type filter, or
// nil when there is none. Values may carry AWS wildcards.
func instanceTypeFilterValues(filters []*ec2.Filter) []string {
	var values []string
	for _, f := range filters {
		if f == nil || aws.StringValue(f.Name) != "instance-type" {
			continue
		}
		if values == nil {
			values = []string{}
		}
		for _, v := range f.Values {
			if v != nil {
				values = append(values, *v)
			}
		}
	}
	return values
}
