package taskdefinition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
)

// Owner is the sole writer of task definition revisions. Every method takes
// the account's ECS bucket, which the caller has already opened.
type Owner struct {
	region string
}

// New returns an Owner minting ARNs in region.
func New(region string) *Owner {
	return &Owner{region: region}
}

// Spec is a validated register request: everything a revision stores except
// its identity, status and registration time.
type Spec struct {
	Family                  string
	NetworkMode             string
	CPU                     string
	Memory                  string
	TaskRoleArn             string
	ExecutionRoleArn        string
	RequiresCompatibilities []string
	RuntimePlatform         *RuntimePlatform
	Containers              []Container
	Tags                    map[string]string
}

// Register stores spec as the family's next revision and advances latest-rev.
func (o *Owner) Register(ctx context.Context, kv jetstream.KeyValue, accountID string, spec Spec) (*Record, error) {
	rev, err := nextRevision(ctx, kv, spec.Family)
	if err != nil {
		return nil, err
	}

	rec := &Record{
		Family:           spec.Family,
		Revision:         rev,
		ARN:              ARN(o.region, accountID, spec.Family, rev),
		NetworkMode:      spec.NetworkMode,
		CPU:              spec.CPU,
		Memory:           spec.Memory,
		TaskRoleArn:      spec.TaskRoleArn,
		ExecutionRoleArn: spec.ExecutionRoleArn,
		Status:           StatusActive,
		Tags:             spec.Tags,
		RegisteredAt:     time.Now().UTC(),
		Containers:       spec.Containers,

		RequiresCompatibilities: spec.RequiresCompatibilities,
		RuntimePlatform:         spec.RuntimePlatform,
	}
	if err := putJSON(ctx, kv, RevKey(spec.Family, rev), rec); err != nil {
		return nil, err
	}
	if err := putJSON(ctx, kv, LatestRevKey(spec.Family), rev); err != nil {
		return nil, err
	}
	return rec, nil
}

// nextRevision reads the family's latest-rev and returns latest+1 (1 if absent).
func nextRevision(ctx context.Context, kv jetstream.KeyValue, family string) (int, error) {
	var latest int
	found, err := getJSON(ctx, kv, LatestRevKey(family), &latest)
	if err != nil {
		return 0, err
	}
	if !found {
		return 1, nil
	}
	return latest + 1, nil
}

// Deregister marks one revision INACTIVE; the revision stays describable.
func (o *Owner) Deregister(ctx context.Context, kv jetstream.KeyValue, family string, rev int) (*Record, error) {
	var rec Record
	found, err := getJSON(ctx, kv, RevKey(family, rev), &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	rec.Status = StatusInactive
	if err := putJSON(ctx, kv, RevKey(family, rev), &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Resolve loads the revision named by ref ("family", "family:rev", or a
// task-definition ARN). A bare family resolves to its latest revision.
func (o *Owner) Resolve(ctx context.Context, kv jetstream.KeyValue, ref string) (*Record, error) {
	family, rev := ParseRef(ref)
	if family == "" {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	if rev == 0 {
		var latest int
		found, err := getJSON(ctx, kv, LatestRevKey(family), &latest)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New(awserrors.ErrorECSInvalidParameter)
		}
		rev = latest
	}
	var rec Record
	found, err := getJSON(ctx, kv, RevKey(family, rev), &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	return &rec, nil
}

// List returns the ARNs of the revisions in status, in key order, limited to
// family when it is set.
func (o *Owner) List(ctx context.Context, kv jetstream.KeyValue, family, status string) ([]string, error) {
	prefix := FamiliesPrefix()
	if family != "" {
		prefix = RevsPrefix(family)
	}
	keys, err := keysWithPrefix(ctx, kv, prefix)
	if err != nil {
		return nil, err
	}
	arns := make([]string, 0, len(keys))
	for _, k := range keys {
		if !strings.Contains(k, "/revs/") {
			continue
		}
		var rec Record
		found, err := getJSON(ctx, kv, k, &rec)
		if err != nil {
			return nil, err
		}
		if found && rec.Status == status {
			arns = append(arns, rec.ARN)
		}
	}
	return arns, nil
}

// getJSON reads key into out. Returns (false, nil) when the key is absent.
func getJSON(ctx context.Context, kv jetstream.KeyValue, key string, out any) (bool, error) {
	entry, err := kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(entry.Value(), out); err != nil {
		return false, fmt.Errorf("unmarshal %s: %w", key, err)
	}
	return true, nil
}

// putJSON marshals v and writes it at key.
func putJSON(ctx context.Context, kv jetstream.KeyValue, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := kv.Put(ctx, key, data); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// keysWithPrefix returns all KV keys under prefix, sorted.
func keysWithPrefix(ctx context.Context, kv jetstream.KeyValue, prefix string) ([]string, error) {
	keys, err := kv.Keys(ctx)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out, nil
}
