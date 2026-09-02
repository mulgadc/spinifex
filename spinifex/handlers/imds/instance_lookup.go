package handlers_imds

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// imdsLookupRetries bounds transient-gather retries for the record-space
// fallback so a momentary JetStream hiccup does not starve guest bootstrap.
const imdsLookupRetries = 3

// retryBackoff sleeps a short increasing delay between IMDS lookup attempts,
// returning false if ctx is cancelled first.
func retryBackoff(ctx context.Context, attempt int) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
		return true
	}
}

// retryGather retries fn on transient error up to imdsLookupRetries times with a
// short backoff, honouring ctx. Returns the last result and error.
func retryGather[T any](ctx context.Context, label, instanceID string, fn func() (*T, error)) (*T, error) {
	var out *T
	var err error
	for attempt := 1; attempt <= imdsLookupRetries; attempt++ {
		out, err = fn()
		if err == nil {
			return out, nil
		}
		if attempt == imdsLookupRetries || !retryBackoff(ctx, attempt) {
			return out, err
		}
		slog.WarnContext(ctx, "IMDS: "+label+" failed, retrying", "instance_id", instanceID, "attempt", attempt, "err", err)
	}
	return out, err
}

// localStateReader reads this node's on-disk VM state. Narrowed to what IMDS
// consumes: it does not know the state lives in a file, so caching and
// invalidation are the implementation's business, not this package's.
type localStateReader interface {
	LocalVM(instanceID string) (*vm.VM, error)
}

// recordLoader is the shared instance record space accessor the fallback
// path needs. Satisfied by *daemon.JetStreamManager; narrowed to an interface
// so the fallback can be exercised without a live JetStream connection.
type recordLoader interface {
	LoadInstanceRecord(instanceID string) (*vm.InstanceRecord, error)
}

// localInstanceLookup resolves instance-only metadata fields local-first: a
// guest is only ever served by the node it runs on, so the node's own on-disk
// VM state is the authoritative, instant answer. It falls back to a direct
// read of the shared instance record space and never fans out over NATS.
type localInstanceLookup struct {
	local   localStateReader // nil-safe: local hit is skipped without it
	records recordLoader     // nil-safe: fallback is skipped without it
}

var _ instanceLookup = (*localInstanceLookup)(nil)

func (l *localInstanceLookup) describe(ctx context.Context, accountID, instanceID string) (*instanceFacts, error) {
	v := l.localVM(ctx, instanceID)
	if v == nil {
		var err error
		v, err = l.recordVM(ctx, instanceID)
		if err != nil {
			return nil, err
		}
	}
	if v == nil || !handlers_ec2_instance.IsInstanceVisibleToCaller(accountID, v) {
		return nil, nil // not present, or not visible to the caller
	}
	return instanceFactsFromVM(v), nil
}

// localVM reads the instance straight off this node's on-disk VM state.
// Returns nil if there is no reader, the instance is not on this node, or the
// read fails — a read failure logs and falls through to the record-space
// fallback rather than erroring the whole lookup.
func (l *localInstanceLookup) localVM(ctx context.Context, instanceID string) *vm.VM {
	if l.local == nil {
		return nil
	}
	v, err := l.local.LocalVM(instanceID)
	if err != nil {
		slog.WarnContext(ctx, "IMDS: local instance state unavailable, falling back to record space", "err", err)
		return nil
	}
	return v
}

// recordVM falls back to the shared instance record space for an instance not
// (yet, or no longer) on this node. Returns (nil, nil) on a genuine miss.
func (l *localInstanceLookup) recordVM(ctx context.Context, instanceID string) (*vm.VM, error) {
	if l.records == nil {
		return nil, nil
	}
	record, err := retryGather(ctx, "load instance record", instanceID, func() (*vm.InstanceRecord, error) {
		return l.records.LoadInstanceRecord(instanceID)
	})
	if err != nil {
		return nil, fmt.Errorf("load instance record %s: %w", instanceID, err)
	}
	if record == nil {
		return nil, nil
	}
	return vm.VMFromRecord(record), nil
}

// instanceFactsFromVM projects the write-once spec fields IMDS serves. Returns
// nil if the instance has no observed instance/reservation yet (mid-launch).
func instanceFactsFromVM(v *vm.VM) *instanceFacts {
	if v.Instance == nil || v.Reservation == nil {
		return nil
	}
	inst := v.Instance

	facts := &instanceFacts{
		instanceType:          v.InstanceType,
		imageID:               aws.StringValue(inst.ImageId),
		keyName:               aws.StringValue(inst.KeyName),
		architecture:          aws.StringValue(inst.Architecture),
		amiLaunchIndex:        aws.Int64Value(inst.AmiLaunchIndex),
		reservationID:         aws.StringValue(v.Reservation.ReservationId),
		iamInstanceProfileArn: v.IamInstanceProfileArn,
		lifecycleType:         v.InstanceLifecycle,
		pendingTime:           aws.TimeValue(inst.LaunchTime),
		userData:              decodeUserData(v.RunInstancesInput),
	}
	if inst.MetadataOptions != nil {
		facts.httpTokens = aws.StringValue(inst.MetadataOptions.HttpTokens)
	}
	return facts
}

// decodeUserData extracts and decodes the launch-time base64 user-data,
// returning nil on absence or a decode error.
func decodeUserData(input *ec2.RunInstancesInput) []byte {
	if input == nil || input.UserData == nil {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(aws.StringValue(input.UserData))
	if err != nil {
		slog.Warn("IMDS: failed to decode instance user-data", "err", err)
		return nil
	}
	return decoded
}
