package handlers_imds

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/daemon"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/utils"
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
	dataDir string
	records recordLoader // nil-safe: fallback is skipped without it

	// readState parses the local state file; overridable in tests to count
	// calls. Defaults to daemon.ReadLocalState.
	readState func(path string) (*daemon.LocalState, error)

	cacheMu    sync.Mutex
	cacheMtime time.Time
	cacheSize  int64
	cached     *daemon.LocalState
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
	if v == nil || !instanceVisible(accountID, v) {
		return nil, nil // not present, or not visible to the caller
	}
	return instanceFactsFromVM(v), nil
}

// instanceVisible replicates the per-node DescribeInstances filtering this
// path bypasses by not going through the daemon's NATS handler: account
// ownership, plus the platform-managed (LB/EKS) hide rule.
func instanceVisible(accountID string, v *vm.VM) bool {
	if !handlers_ec2_instance.IsInstanceVisible(accountID, v.AccountID) {
		return false
	}
	if v.ManagedBy != "" && accountID != utils.GlobalAccountID {
		return false
	}
	return true
}

// localVM reads the instance straight off this node's on-disk VM state,
// through a cache. Returns nil if the file is absent, the instance is not on
// it, or the read fails — a read failure logs and falls through to the
// record-space fallback rather than serving a stale cached hit.
func (l *localInstanceLookup) localVM(ctx context.Context, instanceID string) *vm.VM {
	state, err := l.readCachedState()
	if err != nil {
		slog.WarnContext(ctx, "IMDS: local instance state unavailable, falling back to record space", "err", err)
		return nil
	}
	if state == nil {
		return nil
	}
	return state.VMS[instanceID]
}

// readCachedState reads the local state file, reusing the last parse while
// the file's mtime and size are unchanged. IMDS is guest-driven and served
// concurrently across per-tap responders, so this is mutex-guarded; a stat or
// parse failure clears the cache and returns the error rather than serving a
// stale hit.
func (l *localInstanceLookup) readCachedState() (*daemon.LocalState, error) {
	l.cacheMu.Lock()
	defer l.cacheMu.Unlock()

	path := daemon.LocalStatePath(l.dataDir)
	info, err := os.Stat(path)
	if err != nil {
		l.cached = nil
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	if l.cached != nil && info.ModTime().Equal(l.cacheMtime) && info.Size() == l.cacheSize {
		return l.cached, nil
	}

	read := l.readState
	if read == nil {
		read = daemon.ReadLocalState
	}
	state, err := read(path)
	if err != nil {
		l.cached = nil
		return nil, err
	}
	if state == nil {
		l.cached = nil
		return nil, nil
	}

	l.cached = state
	l.cacheMtime = info.ModTime()
	l.cacheSize = info.Size()
	return state, nil
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
