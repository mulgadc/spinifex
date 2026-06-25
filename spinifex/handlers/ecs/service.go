package handlers_ecs

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
)

// ECSService is the ECS control-plane contract implemented by the daemon and
// adapted onto NATS by NATSECSService. One method per wired AWS ECS action; the
// remaining actions stay NotImplemented at the gateway.
type ECSService interface {
	CreateCluster(input *ecs.CreateClusterInput, accountID string) (*ecs.CreateClusterOutput, error)
	DescribeClusters(input *ecs.DescribeClustersInput, accountID string) (*ecs.DescribeClustersOutput, error)
	ListClusters(input *ecs.ListClustersInput, accountID string) (*ecs.ListClustersOutput, error)

	RegisterTaskDefinition(input *ecs.RegisterTaskDefinitionInput, accountID string) (*ecs.RegisterTaskDefinitionOutput, error)
	DescribeTaskDefinition(input *ecs.DescribeTaskDefinitionInput, accountID string) (*ecs.DescribeTaskDefinitionOutput, error)
	ListTaskDefinitions(input *ecs.ListTaskDefinitionsInput, accountID string) (*ecs.ListTaskDefinitionsOutput, error)

	RegisterContainerInstance(input *ecs.RegisterContainerInstanceInput, accountID string) (*ecs.RegisterContainerInstanceOutput, error)
	DescribeContainerInstances(input *ecs.DescribeContainerInstancesInput, accountID string) (*ecs.DescribeContainerInstancesOutput, error)
	ListContainerInstances(input *ecs.ListContainerInstancesInput, accountID string) (*ecs.ListContainerInstancesOutput, error)

	RunTask(input *ecs.RunTaskInput, accountID string) (*ecs.RunTaskOutput, error)
	StartTask(input *ecs.StartTaskInput, accountID string) (*ecs.StartTaskOutput, error)
	StopTask(input *ecs.StopTaskInput, accountID string) (*ecs.StopTaskOutput, error)
	DescribeTasks(input *ecs.DescribeTasksInput, accountID string) (*ecs.DescribeTasksOutput, error)
	ListTasks(input *ecs.ListTasksInput, accountID string) (*ecs.ListTasksOutput, error)

	CreateService(input *ecs.CreateServiceInput, accountID string) (*ecs.CreateServiceOutput, error)
	UpdateService(input *ecs.UpdateServiceInput, accountID string) (*ecs.UpdateServiceOutput, error)
	DeleteService(input *ecs.DeleteServiceInput, accountID string) (*ecs.DeleteServiceOutput, error)
	DescribeServices(input *ecs.DescribeServicesInput, accountID string) (*ecs.DescribeServicesOutput, error)
	ListServices(input *ecs.ListServicesInput, accountID string) (*ecs.ListServicesOutput, error)

	SubmitTaskStateChange(input *ecs.SubmitTaskStateChangeInput, accountID string) (*ecs.SubmitTaskStateChangeOutput, error)

	// PollAssignments drains an instance's task-assignment inbox (agent → gateway).
	PollAssignments(input *PollAssignmentsInput, accountID string) (*PollAssignmentsOutput, error)
}

// Service is the daemon-side ECS control-plane implementation, backed by the
// per-account JetStream KV bucket. It publishes task assignments on the Layer-2
// bus; the scheduler goroutine consumes instance/task-state events.
type Service struct {
	nc     *nats.Conn
	region string
	suffix string
	// eni owns the awsvpc task-ENI control-plane (create/attach/detach/delete).
	// Defaults to the NATS-backed controller; tests substitute a stub.
	eni eniController
	// targets registers/deregisters service tasks with ELBv2 target groups.
	// Defaults to the NATS-backed registrar; tests substitute a stub.
	targets targetRegistrar
}

var _ ECSService = (*Service)(nil)

// NewService constructs a Service bound to a NATS connection. region scopes the
// ARNs it mints; suffix is the AWS-parity internal DNS suffix (reserved for ECR
// endpoint composition).
func NewService(nc *nats.Conn, region, suffix string) *Service {
	return &Service{nc: nc, region: region, suffix: suffix, eni: newNATSENIController(nc), targets: newNATSTargetRegistrar(nc)}
}

// defaultCluster is the implicit cluster name when a request omits one.
const defaultCluster = "default"

func (s *Service) js() (nats.JetStreamContext, error) {
	if s.nc == nil {
		return nil, errors.New("ecs service: nil nats connection")
	}
	return s.nc.JetStream()
}

// bucket returns the per-account KV handle, creating it on first use.
func (s *Service) bucket(accountID string) (nats.KeyValue, error) {
	js, err := s.js()
	if err != nil {
		return nil, err
	}
	return GetOrCreateAccountBucket(js, accountID)
}

// getJSON reads key into out. Returns (false, nil) when the key is absent.
func getJSON(kv nats.KeyValue, key string, out any) (bool, error) {
	entry, err := kv.Get(key)
	if errors.Is(err, nats.ErrKeyNotFound) {
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
func putJSON(kv nats.KeyValue, key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := kv.Put(key, data); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// keysWithPrefix returns all KV keys under prefix.
func keysWithPrefix(kv nats.KeyValue, prefix string) ([]string, error) {
	keys, err := kv.Keys()
	if errors.Is(err, nats.ErrNoKeysFound) {
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
	sort.Strings(out)
	return out, nil
}

// --- Cluster ---

// CreateCluster persists a cluster meta record. Idempotent: re-creating an
// existing cluster returns the existing ACTIVE record.
func (s *Service) CreateCluster(input *ecs.CreateClusterInput, accountID string) (*ecs.CreateClusterOutput, error) {
	name := aws.StringValue(input.ClusterName)
	if name == "" {
		name = defaultCluster
	}
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}

	var rec ClusterRecord
	found, err := getJSON(kv, ClusterMetaKey(name), &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		rec = ClusterRecord{
			Name:      name,
			ARN:       ClusterARN(s.region, accountID, name),
			Status:    ClusterStatusActive,
			CreatedAt: time.Now().UTC(),
		}
		if len(input.Tags) > 0 {
			rec.Tags = map[string]string{}
			for _, t := range input.Tags {
				rec.Tags[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
			}
		}
		if err := putJSON(kv, ClusterMetaKey(name), &rec); err != nil {
			return nil, err
		}
	}
	return &ecs.CreateClusterOutput{Cluster: rec.toAWS()}, nil
}

// DescribeClusters returns meta for the named clusters; unknown names are
// silently skipped (AWS returns them under "failures", omitted here in v1).
func (s *Service) DescribeClusters(input *ecs.DescribeClustersInput, accountID string) (*ecs.DescribeClustersOutput, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	names := awsStringSlice(input.Clusters)
	if len(names) == 0 {
		names = []string{defaultCluster}
	}
	out := &ecs.DescribeClustersOutput{}
	for _, name := range names {
		name = clusterShortName(name)
		var rec ClusterRecord
		found, err := getJSON(kv, ClusterMetaKey(name), &rec)
		if err != nil {
			return nil, err
		}
		if found {
			out.Clusters = append(out.Clusters, rec.toAWS())
		}
	}
	return out, nil
}

// ListClusters returns the ARNs of all clusters in the account.
func (s *Service) ListClusters(_ *ecs.ListClustersInput, accountID string) (*ecs.ListClustersOutput, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	keys, err := keysWithPrefix(kv, "clusters/")
	if err != nil {
		return nil, err
	}
	out := &ecs.ListClustersOutput{}
	for _, k := range keys {
		if !strings.HasSuffix(k, "/meta") {
			continue
		}
		var rec ClusterRecord
		found, err := getJSON(kv, k, &rec)
		if err != nil {
			return nil, err
		}
		if found {
			out.ClusterArns = append(out.ClusterArns, aws.String(rec.ARN))
		}
	}
	return out, nil
}

func (r *ClusterRecord) toAWS() *ecs.Cluster {
	return &ecs.Cluster{
		ClusterName: aws.String(r.Name),
		ClusterArn:  aws.String(r.ARN),
		Status:      aws.String(r.Status),
	}
}

// --- Task definition ---

// RegisterTaskDefinition stores a new revision of a family, bumping latest-rev.
func (s *Service) RegisterTaskDefinition(input *ecs.RegisterTaskDefinitionInput, accountID string) (*ecs.RegisterTaskDefinitionOutput, error) {
	family := aws.StringValue(input.Family)
	if family == "" {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}

	rev, err := s.nextRevision(kv, family)
	if err != nil {
		return nil, err
	}

	rec := TaskDefRecord{
		Family:       family,
		Revision:     rev,
		ARN:          TaskDefARN(s.region, accountID, family, rev),
		NetworkMode:  aws.StringValue(input.NetworkMode),
		CPU:          aws.StringValue(input.Cpu),
		Memory:       aws.StringValue(input.Memory),
		Status:       TaskDefStatusActive,
		RegisteredAt: time.Now().UTC(),
		Containers:   containerDefsFromAWS(input.ContainerDefinitions),
	}
	if err := putJSON(kv, TaskDefRevKey(family, rev), &rec); err != nil {
		return nil, err
	}
	if err := putJSON(kv, TaskDefLatestRevKey(family), rev); err != nil {
		return nil, err
	}
	return &ecs.RegisterTaskDefinitionOutput{TaskDefinition: rec.toAWS()}, nil
}

// nextRevision reads the family's latest-rev and returns latest+1 (1 if absent).
func (s *Service) nextRevision(kv nats.KeyValue, family string) (int, error) {
	var latest int
	found, err := getJSON(kv, TaskDefLatestRevKey(family), &latest)
	if err != nil {
		return 0, err
	}
	if !found {
		return 1, nil
	}
	return latest + 1, nil
}

// DescribeTaskDefinition resolves "family", "family:rev" or an ARN to a revision.
func (s *Service) DescribeTaskDefinition(input *ecs.DescribeTaskDefinitionInput, accountID string) (*ecs.DescribeTaskDefinitionOutput, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	rec, err := s.resolveTaskDef(kv, aws.StringValue(input.TaskDefinition))
	if err != nil {
		return nil, err
	}
	return &ecs.DescribeTaskDefinitionOutput{TaskDefinition: rec.toAWS()}, nil
}

// ListTaskDefinitions returns all revision ARNs, optionally filtered by family.
func (s *Service) ListTaskDefinitions(input *ecs.ListTaskDefinitionsInput, accountID string) (*ecs.ListTaskDefinitionsOutput, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	prefix := TaskDefFamiliesPrefix()
	if fam := aws.StringValue(input.FamilyPrefix); fam != "" {
		prefix = TaskDefRevsPrefix(fam)
	}
	keys, err := keysWithPrefix(kv, prefix)
	if err != nil {
		return nil, err
	}
	out := &ecs.ListTaskDefinitionsOutput{}
	for _, k := range keys {
		if !strings.Contains(k, "/revs/") {
			continue
		}
		var rec TaskDefRecord
		found, err := getJSON(kv, k, &rec)
		if err != nil {
			return nil, err
		}
		if found {
			out.TaskDefinitionArns = append(out.TaskDefinitionArns, aws.String(rec.ARN))
		}
	}
	return out, nil
}

// resolveTaskDef loads the TaskDefRecord named by ref ("family", "family:rev",
// or a task-definition ARN). A bare family resolves to its latest revision.
func (s *Service) resolveTaskDef(kv nats.KeyValue, ref string) (*TaskDefRecord, error) {
	family, rev := parseTaskDefRef(ref)
	if family == "" {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	if rev == 0 {
		var latest int
		found, err := getJSON(kv, TaskDefLatestRevKey(family), &latest)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errors.New(awserrors.ErrorECSInvalidParameter)
		}
		rev = latest
	}
	var rec TaskDefRecord
	found, err := getJSON(kv, TaskDefRevKey(family, rev), &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	return &rec, nil
}

// parseTaskDefRef splits "family", "family:rev" or an ARN into (family, rev).
// rev is 0 when unspecified (caller resolves to latest).
func parseTaskDefRef(ref string) (string, int) {
	ref = strings.TrimSpace(ref)
	if i := strings.LastIndex(ref, "task-definition/"); i >= 0 {
		ref = ref[i+len("task-definition/"):]
	}
	family := ref
	rev := 0
	if i := strings.LastIndexByte(ref, ':'); i >= 0 {
		family = ref[:i]
		if n, err := strconv.Atoi(ref[i+1:]); err == nil {
			rev = n
		}
	}
	return family, rev
}

func (r *TaskDefRecord) toAWS() *ecs.TaskDefinition {
	td := &ecs.TaskDefinition{
		Family:            aws.String(r.Family),
		Revision:          aws.Int64(int64(r.Revision)),
		TaskDefinitionArn: aws.String(r.ARN),
		Status:            aws.String(r.Status),
	}
	if r.NetworkMode != "" {
		td.NetworkMode = aws.String(r.NetworkMode)
	}
	if r.CPU != "" {
		td.Cpu = aws.String(r.CPU)
	}
	if r.Memory != "" {
		td.Memory = aws.String(r.Memory)
	}
	for _, c := range r.Containers {
		td.ContainerDefinitions = append(td.ContainerDefinitions, c.toAWS())
	}
	return td
}
