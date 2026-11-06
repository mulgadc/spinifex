package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/nats-io/nats.go/jetstream"
)

// ClusterRecordState is what a read of a cluster's record established.
type ClusterRecordState int

const (
	ClusterRecordPresent ClusterRecordState = iota
	ClusterRecordMissing
	ClusterRecordUnreadable
)

// ClusterRecordVerdict is the outcome of looking a cluster record up without
// creating anything. Reason names a missing or unreadable record; Err is set
// only when the record could not be read.
type ClusterRecordVerdict struct {
	State  ClusterRecordState
	Reason string
	Err    error
}

// JudgeClusterRecord reads the cluster record read-only. A missing bucket or a
// missing key is Missing, any other failure is Unreadable, and neither is ever
// repaired. The billable reaper and the orphan report both use it, so they
// judge a record the same way.
func JudgeClusterRecord(ctx context.Context, js jetstream.JetStream, account, cluster string) ClusterRecordVerdict {
	kv, err := OpenAccountBucket(ctx, js, account)
	switch {
	case errors.Is(err, jetstream.ErrBucketNotFound):
		return ClusterRecordVerdict{State: ClusterRecordMissing, Reason: "account bucket missing"}
	case err != nil:
		return ClusterRecordVerdict{State: ClusterRecordUnreadable, Reason: "account bucket lookup failed", Err: err}
	}
	_, err = GetClusterMeta(ctx, kv, cluster)
	switch {
	case err == nil:
		return ClusterRecordVerdict{State: ClusterRecordPresent}
	case errors.Is(err, ErrClusterNotFound):
		return ClusterRecordVerdict{State: ClusterRecordMissing, Reason: "cluster record missing"}
	}
	return ClusterRecordVerdict{State: ClusterRecordUnreadable, Reason: "cluster record read failed", Err: err}
}

// ENITagReader returns the tags on a control-plane ENI. found is false when
// the ENI is gone.
type ENITagReader func(ctx context.Context, eniID string) (tagSet map[string]string, found bool, err error)

// ControlPlaneRow is one running EKS control-plane VM the report could not tie
// to a live cluster record. Unreadable rows carry Err and are never omitted.
type ControlPlaneRow struct {
	InstanceID       string
	Cluster          string
	Account          string
	Reason           string
	Age              time.Duration
	AvailabilityZone string
	Unreadable       bool
	Err              error
}

// FindControlPlaneOrphans classifies the instances in reservations. An instance
// with no ENI, or whose ENI is gone or not tagged as an EKS control plane, is
// skipped. A failed ENI read is an unreadable row, never a silent drop. A
// control plane whose cluster record is present is omitted; one whose bucket or
// record is missing is an orphan row; any other read failure is unreadable. It
// only reads.
func FindControlPlaneOrphans(ctx context.Context, js jetstream.JetStream, reservations []*ec2.Reservation, eni ENITagReader, now time.Time) []ControlPlaneRow {
	var rows []ControlPlaneRow
	for _, res := range reservations {
		if res == nil {
			continue
		}
		for _, inst := range res.Instances {
			if inst == nil || !instanceRunning(inst) || len(inst.NetworkInterfaces) == 0 || inst.NetworkInterfaces[0] == nil {
				continue
			}
			eniID := aws.StringValue(inst.NetworkInterfaces[0].NetworkInterfaceId)
			if eniID == "" {
				continue
			}
			row := ControlPlaneRow{InstanceID: aws.StringValue(inst.InstanceId)}
			if inst.Placement != nil {
				row.AvailabilityZone = aws.StringValue(inst.Placement.AvailabilityZone)
			}
			if inst.LaunchTime != nil {
				row.Age = now.Sub(*inst.LaunchTime).Truncate(time.Second)
			}

			tagSet, found, err := eni(ctx, eniID)
			if err != nil {
				row.Unreadable, row.Err, row.Reason = true, err, "control-plane ENI read failed"
				rows = append(rows, row)
				continue
			}
			if !found || tagSet[tags.ManagedByKey] != tags.ManagedByEKS || tagSet[clusterEKSRoleTagKey] != clusterEKSRoleControlPlane {
				continue
			}
			row.Cluster, row.Account = tagSet[clusterEKSClusterTagKey], tagSet[clusterEKSAccountTagKey]
			if row.Cluster == "" || row.Account == "" {
				continue
			}

			verdict := JudgeClusterRecord(ctx, js, row.Account, row.Cluster)
			switch verdict.State {
			case ClusterRecordPresent:
				continue
			case ClusterRecordUnreadable:
				row.Unreadable, row.Err = true, verdict.Err
			}
			row.Reason = verdict.Reason
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].InstanceID < rows[j].InstanceID })
	return rows
}

// ReportControlPlaneOrphans writes the orphan report. list must return every
// non-terminated system instance or an error: a failed or partial listing is
// returned as an error and nothing is printed, so a short list is never taken
// for a complete one. Unreadable rows are printed and counted separately.
func ReportControlPlaneOrphans(ctx context.Context, w io.Writer, js jetstream.JetStream, list func(context.Context) (*ec2.DescribeInstancesOutput, error), eni ENITagReader, now time.Time) (orphans, unreadable int, err error) {
	out, err := list(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("listing incomplete: %w", err)
	}
	if out == nil {
		return 0, 0, errors.New("listing incomplete: no result")
	}
	rows := FindControlPlaneOrphans(ctx, js, out.Reservations, eni, now)
	for _, r := range rows {
		if r.Unreadable {
			unreadable++
			fmt.Fprintf(w, "UNREADABLE  %s  cluster=%s account=%s az=%s age=%s  %s: %v\n", r.InstanceID, r.Cluster, r.Account, r.AvailabilityZone, r.Age, r.Reason, r.Err)
			continue
		}
		orphans++
		fmt.Fprintf(w, "ORPHAN      %s  cluster=%s account=%s az=%s age=%s  %s\n", r.InstanceID, r.Cluster, r.Account, r.AvailabilityZone, r.Age, r.Reason)
	}
	return orphans, unreadable, nil
}

func instanceRunning(inst *ec2.Instance) bool {
	return inst.State != nil && aws.StringValue(inst.State.Name) == ec2.InstanceStateNameRunning
}
