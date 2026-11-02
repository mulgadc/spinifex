package instance

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// instanceIDPattern is the AWS instance ID shape: i- followed by 8 (legacy)
// or 17 lowercase hex characters.
var instanceIDPattern = regexp.MustCompile(`^i-([0-9a-f]{8}|[0-9a-f]{17})$`)

// validateInstanceIDFormat rejects the whole batch with InvalidInstanceID.Malformed
// if any ID is not a well-formed instance ID. Nil entries are skipped, as the
// lifecycle handlers skip them.
func validateInstanceIDFormat(ids []*string) error {
	for _, id := range ids {
		if id != nil && !instanceIDPattern.MatchString(*id) {
			return awserrors.Errorf(awserrors.ErrorInvalidInstanceIDMalformed, "Invalid id: %q", *id)
		}
	}
	return nil
}

// cmdRetrySleep is the backoff seam between NoResponders retries; tests override it.
var cmdRetrySleep = time.Sleep

// instanceCmdTimeout bounds each ec2.cmd.<id> request; tests shorten it to
// exercise ErrTimeout without a real wait.
var instanceCmdTimeout = 5 * time.Second

// requestInstanceCmd sends a command on ec2.cmd.<id>, retrying ErrNoResponders
// because the owner's per-instance subscription may not have propagated yet
// after a cluster restart. Only an ErrNoResponders that survives every retry
// suggests no daemon owns the instance.
func requestInstanceCmd(ctx context.Context, natsConn *nats.Conn, instanceID, accountID string, data []byte) (*nats.Msg, error) {
	subject := ec2v1.InstanceCommandSubject(instanceID)
	var (
		msg *nats.Msg
		err error
	)
	for attempt := range 3 {
		reqMsg := nats.NewMsg(subject)
		reqMsg.Data = data
		reqMsg.Header.Set(natsmsg.AccountIDHeader, accountID)
		natsmsg.InjectTraceContext(ctx, reqMsg.Header)
		msg, err = natsConn.RequestMsg(reqMsg, instanceCmdTimeout)
		if err == nil || !errors.Is(err, nats.ErrNoResponders) {
			return msg, err
		}
		if attempt < 2 {
			slog.DebugContext(ctx, "No responder on per-instance topic, retrying",
				"instance_id", instanceID, "attempt", attempt+1)
			cmdRetrySleep(time.Duration(attempt+1) * time.Second)
		}
	}
	return msg, err
}

// Bucket states reported by classifyUnownedInstance.
const (
	bucketStateAbsent     = ""
	bucketStateStopped    = "stopped"
	bucketStateTerminated = "terminated"
)

// classifyUnownedInstance looks up an instance no daemon answered for in the
// terminated and stopped KV buckets. A hit in either is returned with ok=true.
// Absence is reported (bucketStateAbsent, true) only when both queries succeeded,
// so a failed query never reads as "not found".
func classifyUnownedInstance(ctx context.Context, natsConn *nats.Conn, instanceID, accountID string) (state string, ok bool) {
	reqData, err := json.Marshal(&ec2.DescribeInstancesInput{InstanceIds: []*string{&instanceID}})
	if err != nil {
		slog.ErrorContext(ctx, "classifyUnownedInstance: failed to marshal request", "instance_id", instanceID, "err", err)
		return bucketStateAbsent, false
	}
	allOK := true
	for _, bucket := range []struct{ topic, state string }{
		{"ec2.DescribeTerminatedInstances", bucketStateTerminated},
		{"ec2.DescribeStoppedInstances", bucketStateStopped},
	} {
		reservations, queryOK := queryInstanceBucket(ctx, natsConn, bucket.topic, reqData, accountID)
		if !queryOK {
			allOK = false
			continue
		}
		if len(missingInstanceIDs(reservations, []*string{&instanceID})) == 0 {
			return bucket.state, true
		}
	}
	return bucketStateAbsent, allOK
}
