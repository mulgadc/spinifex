package gateway_ec2_instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	notApplicable          = "not-applicable"
	reachabilityDetailName = "reachability"
)

// DescribeInstanceStatus fans out to every node and, with IncludeAllInstances,
// also augments from the stopped-instance KV bucket. The aggregator propagates
// the first deterministic 4xx only when no data was collected, matching
// DescribeInstances.
func DescribeInstanceStatus(input *ec2.DescribeInstanceStatusInput, natsConn *nats.Conn, expectedNodes int, accountID, az string) (*ec2.DescribeInstanceStatusOutput, error) {
	if input == nil {
		input = &ec2.DescribeInstanceStatusInput{}
	}

	jsonData, err := json.Marshal(input)
	if err != nil {
		slog.Error("DescribeInstanceStatus: Failed to marshal input", "err", err)
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	frames, sum, err := utils.Gather(natsConn, "ec2.DescribeInstanceStatus", jsonData,
		utils.GatherOpts{Timeout: 3 * time.Second, ExpectedNodes: expectedNodes, AccountID: accountID})
	if err != nil {
		return nil, err
	}

	var allStatuses []*ec2.InstanceStatus
	for _, frame := range frames {
		var nodeOutput ec2.DescribeInstanceStatusOutput
		if json.Unmarshal(frame, &nodeOutput) == nil {
			allStatuses = append(allStatuses, nodeOutput.InstanceStatuses...)
		}
	}

	if aws.BoolValue(input.IncludeAllInstances) {
		if stopped := queryStoppedInstancesForStatus(natsConn, input, accountID, az); len(stopped) > 0 {
			allStatuses = append(allStatuses, stopped...)
		}
	}

	finalStatuses := dedupStatuses(allStatuses)

	if sum.FirstClient4xx != "" && len(finalStatuses) == 0 {
		return nil, errors.New(sum.FirstClient4xx)
	}

	slog.Info("DescribeInstanceStatus: Aggregated response", "total_statuses", len(finalStatuses))
	return &ec2.DescribeInstanceStatusOutput{InstanceStatuses: finalStatuses}, nil
}

// queryStoppedInstancesForStatus projects input to DescribeInstancesInput for the stopped-KV handler.
// IncludeAllInstances and status-specific filters are stripped (the handler rejects them);
// those filters are re-applied gateway-side on the returned statuses.
func queryStoppedInstancesForStatus(natsConn *nats.Conn, input *ec2.DescribeInstanceStatusInput, accountID, az string) []*ec2.InstanceStatus {
	projected := &ec2.DescribeInstancesInput{
		InstanceIds: input.InstanceIds,
		Filters:     stoppedCompatibleFilters(input.Filters),
	}
	jsonData, err := json.Marshal(projected)
	if err != nil {
		slog.Error("DescribeInstanceStatus: failed to marshal stopped query", "err", err)
		return nil
	}
	reservations := queryInstanceBucket(natsConn, "ec2.DescribeStoppedInstances", jsonData, accountID)
	var statuses []*ec2.InstanceStatus
	for _, res := range reservations {
		if res == nil {
			continue
		}
		for _, inst := range res.Instances {
			if inst == nil || inst.InstanceId == nil {
				continue
			}
			statuses = append(statuses, buildInstanceStatusFromInstance(inst, az))
		}
	}
	return filterStoppedStatuses(statuses, input.Filters, az)
}

// stoppedCompatibleFilters strips filters the stopped-instance handler would
// reject (its valid-filter map differs from DescribeInstanceStatus's).
func stoppedCompatibleFilters(filters []*ec2.Filter) []*ec2.Filter {
	if len(filters) == 0 {
		return nil
	}
	out := make([]*ec2.Filter, 0, len(filters))
	for _, f := range filters {
		if f == nil || f.Name == nil {
			continue
		}
		switch *f.Name {
		case "availability-zone", "instance-state-code":
			continue
		}
		out = append(out, f)
	}
	return out
}

// filterStoppedStatuses applies DescribeInstanceStatus-specific filters that
// could not be forwarded to the stopped handler.
func filterStoppedStatuses(in []*ec2.InstanceStatus, filters []*ec2.Filter, az string) []*ec2.InstanceStatus {
	if len(in) == 0 || len(filters) == 0 {
		return in
	}
	out := in[:0]
	for _, s := range in {
		keep := true
		for _, f := range filters {
			if f == nil || f.Name == nil {
				continue
			}
			switch *f.Name {
			case "availability-zone":
				if !filterValueMatches(f.Values, az) {
					keep = false
				}
			case "instance-state-code":
				code := ""
				if s.InstanceState != nil && s.InstanceState.Code != nil {
					code = strconv.FormatInt(*s.InstanceState.Code, 10)
				}
				if !filterValueMatches(f.Values, code) {
					keep = false
				}
			}
			if !keep {
				break
			}
		}
		if keep {
			out = append(out, s)
		}
	}
	return out
}

func filterValueMatches(values []*string, field string) bool {
	for _, v := range values {
		if v != nil && *v == field {
			return true
		}
	}
	return false
}

func buildInstanceStatusFromInstance(inst *ec2.Instance, az string) *ec2.InstanceStatus {
	state := &ec2.InstanceState{Name: aws.String(ec2.InstanceStateNameStopped)}
	if inst.State != nil {
		if inst.State.Name != nil {
			state.Name = inst.State.Name
		}
		state.Code = inst.State.Code
	}

	return &ec2.InstanceStatus{
		AvailabilityZone: aws.String(az),
		InstanceId:       inst.InstanceId,
		InstanceState:    state,
		InstanceStatus: &ec2.InstanceStatusSummary{
			Status: aws.String(notApplicable),
			Details: []*ec2.InstanceStatusDetails{{
				Name:   aws.String(reachabilityDetailName),
				Status: aws.String(notApplicable),
			}},
		},
		SystemStatus: &ec2.InstanceStatusSummary{
			Status: aws.String(notApplicable),
			Details: []*ec2.InstanceStatusDetails{{
				Name:   aws.String(reachabilityDetailName),
				Status: aws.String(notApplicable),
			}},
		},
	}
}

// First-writer-wins by InstanceId, so a running fan-out entry naturally beats a
// stale stopped-KV entry during stop/start race windows.
func dedupStatuses(in []*ec2.InstanceStatus) []*ec2.InstanceStatus {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]*ec2.InstanceStatus, 0, len(in))
	for _, s := range in {
		if s == nil || s.InstanceId == nil {
			continue
		}
		id := *s.InstanceId
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, s)
	}
	return out
}
