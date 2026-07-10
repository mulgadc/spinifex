package handlers_ecs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
)

// ECSService is the ECS control-plane contract implemented by the daemon and
// adapted onto NATS by NATSECSService. One method per wired AWS ECS action; the
// remaining actions stay NotImplemented at the gateway.
type ECSService interface {
	CreateCluster(ctx context.Context, input *ecs.CreateClusterInput, accountID string) (*ecs.CreateClusterOutput, error)
	DeleteCluster(ctx context.Context, input *ecs.DeleteClusterInput, accountID string) (*ecs.DeleteClusterOutput, error)
	DescribeClusters(ctx context.Context, input *ecs.DescribeClustersInput, accountID string) (*ecs.DescribeClustersOutput, error)
	ListClusters(ctx context.Context, input *ecs.ListClustersInput, accountID string) (*ecs.ListClustersOutput, error)

	RegisterTaskDefinition(ctx context.Context, input *ecs.RegisterTaskDefinitionInput, accountID string) (*ecs.RegisterTaskDefinitionOutput, error)
	DeregisterTaskDefinition(ctx context.Context, input *ecs.DeregisterTaskDefinitionInput, accountID string) (*ecs.DeregisterTaskDefinitionOutput, error)
	DescribeTaskDefinition(ctx context.Context, input *ecs.DescribeTaskDefinitionInput, accountID string) (*ecs.DescribeTaskDefinitionOutput, error)
	ListTaskDefinitions(ctx context.Context, input *ecs.ListTaskDefinitionsInput, accountID string) (*ecs.ListTaskDefinitionsOutput, error)

	RegisterContainerInstance(ctx context.Context, input *ecs.RegisterContainerInstanceInput, accountID string) (*ecs.RegisterContainerInstanceOutput, error)
	DeregisterContainerInstance(ctx context.Context, input *ecs.DeregisterContainerInstanceInput, accountID string) (*ecs.DeregisterContainerInstanceOutput, error)
	UpdateContainerInstancesState(ctx context.Context, input *ecs.UpdateContainerInstancesStateInput, accountID string) (*ecs.UpdateContainerInstancesStateOutput, error)
	DescribeContainerInstances(ctx context.Context, input *ecs.DescribeContainerInstancesInput, accountID string) (*ecs.DescribeContainerInstancesOutput, error)
	ListContainerInstances(ctx context.Context, input *ecs.ListContainerInstancesInput, accountID string) (*ecs.ListContainerInstancesOutput, error)

	RunTask(ctx context.Context, input *ecs.RunTaskInput, accountID string) (*ecs.RunTaskOutput, error)
	StartTask(ctx context.Context, input *ecs.StartTaskInput, accountID string) (*ecs.StartTaskOutput, error)
	StopTask(ctx context.Context, input *ecs.StopTaskInput, accountID string) (*ecs.StopTaskOutput, error)
	DescribeTasks(ctx context.Context, input *ecs.DescribeTasksInput, accountID string) (*ecs.DescribeTasksOutput, error)
	ListTasks(ctx context.Context, input *ecs.ListTasksInput, accountID string) (*ecs.ListTasksOutput, error)

	CreateService(ctx context.Context, input *ecs.CreateServiceInput, accountID string) (*ecs.CreateServiceOutput, error)
	UpdateService(ctx context.Context, input *ecs.UpdateServiceInput, accountID string) (*ecs.UpdateServiceOutput, error)
	DeleteService(ctx context.Context, input *ecs.DeleteServiceInput, accountID string) (*ecs.DeleteServiceOutput, error)
	DescribeServices(ctx context.Context, input *ecs.DescribeServicesInput, accountID string) (*ecs.DescribeServicesOutput, error)
	ListServices(ctx context.Context, input *ecs.ListServicesInput, accountID string) (*ecs.ListServicesOutput, error)

	SubmitTaskStateChange(ctx context.Context, input *ecs.SubmitTaskStateChangeInput, accountID string) (*ecs.SubmitTaskStateChangeOutput, error)

	// PollAssignments drains an instance's task-assignment inbox (agent → gateway).
	PollAssignments(ctx context.Context, input *PollAssignmentsInput, accountID string) (*PollAssignmentsOutput, error)

	// ProvisionCapacity launches container-instance EC2 capacity into a cluster.
	ProvisionCapacity(ctx context.Context, input *ProvisionCapacityInput, accountID string) (*ProvisionCapacityOutput, error)

	// Tags. Dispatched on the resourceArn shape (ecs-v1.md §1); the ACM inline-tags
	// pattern (map stored on the record, mutated in place).
	ListTagsForResource(ctx context.Context, input *ecs.ListTagsForResourceInput, accountID string) (*ecs.ListTagsForResourceOutput, error)
	TagResource(ctx context.Context, input *ecs.TagResourceInput, accountID string) (*ecs.TagResourceOutput, error)
	UntagResource(ctx context.Context, input *ecs.UntagResourceInput, accountID string) (*ecs.UntagResourceOutput, error)

	// Capacity providers. Strategy is accepted and persisted but inert: no
	// scheduler coupling, no scale loop (a separate follow-on binds this to an
	// ASG primitive).
	PutClusterCapacityProviders(ctx context.Context, input *ecs.PutClusterCapacityProvidersInput, accountID string) (*ecs.PutClusterCapacityProvidersOutput, error)
	CreateCapacityProvider(ctx context.Context, input *ecs.CreateCapacityProviderInput, accountID string) (*ecs.CreateCapacityProviderOutput, error)
	DescribeCapacityProviders(ctx context.Context, input *ecs.DescribeCapacityProvidersInput, accountID string) (*ecs.DescribeCapacityProvidersOutput, error)
	DeleteCapacityProvider(ctx context.Context, input *ecs.DeleteCapacityProviderInput, accountID string) (*ecs.DeleteCapacityProviderOutput, error)
}

// ecsImageResolver is the narrow AMI surface for resolving the spinifex-ecs-node
// AMI by tag.
type ecsImageResolver interface {
	DescribeImages(ctx context.Context, input *ec2.DescribeImagesInput, accountID string) (*ec2.DescribeImagesOutput, error)
}

// ecsIAM is the narrow IAM surface ProvisionCapacity needs to find-or-create the
// ecsInstanceRole, its ecs:* policy, and the instance profile workers expose
// over IMDS. Method signatures match the daemon IAM service implementation.
type ecsIAM interface {
	GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error)
	CreateRole(accountID string, input *iam.CreateRoleInput) (*iam.CreateRoleOutput, error)
	CreatePolicy(accountID string, input *iam.CreatePolicyInput) (*iam.CreatePolicyOutput, error)
	AttachRolePolicy(accountID string, input *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error)
	GetInstanceProfile(accountID string, input *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error)
	CreateInstanceProfile(accountID string, input *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error)
	AddRoleToInstanceProfile(accountID string, input *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error)
}

// Deps are the collaborators ProvisionCapacity needs: the gateway endpoint/CA to
// seed the agent's config, the IAM service to back the instance profile, the
// image resolver for the AMI, and the customer RunInstances path. Nil deps
// disable capacity provisioning; the wired actions stay usable.
type Deps struct {
	GatewayBaseURL string
	GatewayCACert  string
	IAM            ecsIAM
	Images         ecsImageResolver
	RunInstances   func(context.Context, *ec2.RunInstancesInput, string) (*ec2.Reservation, error)
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
	// eips allocates/associates and releases an Elastic IP for awsvpc tasks whose
	// service has AssignPublicIp=ENABLED. Defaults to the NATS-backed manager;
	// tests substitute a stub.
	eips eipManager
	// deps carries the collaborators ProvisionCapacity needs (gateway endpoint/CA,
	// IAM service, image resolver, customer RunInstances path). Wired via WithDeps.
	deps Deps
}

var _ ECSService = (*Service)(nil)

// WithDeps attaches the ProvisionCapacity collaborators and returns the Service
// for chaining. Non-breaking: NewService stays usable without deps.
func (s *Service) WithDeps(d Deps) *Service {
	s.deps = d
	return s
}

// NewService constructs a Service bound to a NATS connection. region scopes the
// ARNs it mints; suffix is the AWS-parity internal DNS suffix (reserved for ECR
// endpoint composition).
func NewService(nc *nats.Conn, region, suffix string) *Service {
	return &Service{nc: nc, region: region, suffix: suffix, eni: newNATSENIController(nc), targets: newNATSTargetRegistrar(nc), eips: newNATSEIPManager(nc)}
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

// deleteKeysWithPrefix deletes every KV key under prefix. Used by DeleteCluster
// to sweep a cluster's whole key subtree (meta/instances/tasks/services/
// assignments) after its tasks are stopped. Best-effort per key; the first
// delete error is returned after attempting the rest.
func deleteKeysWithPrefix(kv nats.KeyValue, prefix string) error {
	keys, err := keysWithPrefix(kv, prefix)
	if err != nil {
		return err
	}
	var firstErr error
	for _, k := range keys {
		if derr := kv.Delete(k); derr != nil && firstErr == nil {
			firstErr = derr
		}
	}
	return firstErr
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
	slices.Sort(out)
	return out, nil
}

// --- Cluster ---

// CreateCluster persists a cluster meta record. Idempotent: re-creating an
// existing cluster returns the existing ACTIVE record.
func (s *Service) CreateCluster(_ context.Context, input *ecs.CreateClusterInput, accountID string) (*ecs.CreateClusterOutput, error) {
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
func (s *Service) DescribeClusters(_ context.Context, input *ecs.DescribeClustersInput, accountID string) (*ecs.DescribeClustersOutput, error) {
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
func (s *Service) ListClusters(_ context.Context, _ *ecs.ListClustersInput, accountID string) (*ecs.ListClustersOutput, error) {
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
	c := &ecs.Cluster{
		ClusterName: aws.String(r.Name),
		ClusterArn:  aws.String(r.ARN),
		Status:      aws.String(r.Status),
		Tags:        tagsToAWS(r.Tags),
	}
	if len(r.CapacityProviders) > 0 {
		c.CapacityProviders = aws.StringSlice(r.CapacityProviders)
	}
	for _, item := range r.DefaultCapacityProviderStrategy {
		c.DefaultCapacityProviderStrategy = append(c.DefaultCapacityProviderStrategy, item.toAWS())
	}
	return c
}

// tagsToAWS converts a stored tag map into the AWS list form with a stable
// key order so Describe output is deterministic.
func tagsToAWS(tags map[string]string) []*ecs.Tag {
	if len(tags) == 0 {
		return nil
	}
	keys := slices.Sorted(maps.Keys(tags))
	out := make([]*ecs.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, &ecs.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	return out
}

// tagsToMap is the inverse of tagsToAWS: it converts the AWS tag list form
// into a stored map, dropping nil entries and empty keys.
func tagsToMap(tags []*ecs.Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		if t == nil || aws.StringValue(t.Key) == "" {
			continue
		}
		out[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// --- Task definition ---

// RegisterTaskDefinition stores a new revision of a family, bumping latest-rev.
func (s *Service) RegisterTaskDefinition(ctx context.Context, input *ecs.RegisterTaskDefinitionInput, accountID string) (*ecs.RegisterTaskDefinitionOutput, error) {
	family := aws.StringValue(input.Family)
	if family == "" {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	if err := validateContainerDefs(input.ContainerDefinitions); err != nil {
		return nil, err
	}
	warnUnsupportedLogDrivers(ctx, family, input.ContainerDefinitions)
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}

	rev, err := s.nextRevision(kv, family)
	if err != nil {
		return nil, err
	}

	rec := TaskDefRecord{
		Family:           family,
		Revision:         rev,
		ARN:              TaskDefARN(s.region, accountID, family, rev),
		NetworkMode:      aws.StringValue(input.NetworkMode),
		CPU:              aws.StringValue(input.Cpu),
		Memory:           aws.StringValue(input.Memory),
		TaskRoleArn:      aws.StringValue(input.TaskRoleArn),
		ExecutionRoleArn: aws.StringValue(input.ExecutionRoleArn),
		Status:           TaskDefStatusActive,
		Tags:             tagsToMap(input.Tags),
		RegisteredAt:     time.Now().UTC(),
		Containers:       containerDefsFromAWS(input.ContainerDefinitions),
	}
	if err := putJSON(kv, TaskDefRevKey(family, rev), &rec); err != nil {
		return nil, err
	}
	if err := putJSON(kv, TaskDefLatestRevKey(family), rev); err != nil {
		return nil, err
	}
	return &ecs.RegisterTaskDefinitionOutput{TaskDefinition: rec.toAWS(), Tags: tagsToAWS(rec.Tags)}, nil
}

// validateContainerDefs hard-rejects taskdef features the data plane cannot honor.
// secrets[] is dropped silently by the assign path, so a task would run believing
// it has secrets it never receives (ecs-v1 Q18) — reject at register instead.
func validateContainerDefs(defs []*ecs.ContainerDefinition) error {
	for _, c := range defs {
		if c != nil && len(c.Secrets) > 0 {
			return errors.New(awserrors.ErrorECSInvalidParameter)
		}
	}
	return nil
}

// warnUnsupportedLogDrivers emits the honest "logs discarded" operator signal for
// any container whose log driver is not the host-side json-file default. The
// taskdef is still accepted for parity; only json-file is actually collected.
func warnUnsupportedLogDrivers(ctx context.Context, family string, defs []*ecs.ContainerDefinition) {
	for _, c := range defs {
		if c == nil || c.LogConfiguration == nil {
			continue
		}
		driver := aws.StringValue(c.LogConfiguration.LogDriver)
		if driver == "" || driver == LogDriverJSONFile {
			continue
		}
		slog.WarnContext(ctx, "ECS RegisterTaskDefinition: logDriver not implemented; container logs will be discarded",
			"family", family, "container", aws.StringValue(c.Name), "logDriver", driver)
	}
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

// DeregisterTaskDefinition marks a specific task-definition revision INACTIVE.
// AWS requires an explicit family:revision (a bare family is rejected); the
// revision stays describable, matching AWS. Idempotent.
func (s *Service) DeregisterTaskDefinition(_ context.Context, input *ecs.DeregisterTaskDefinitionInput, accountID string) (*ecs.DeregisterTaskDefinitionOutput, error) {
	family, rev := parseTaskDefRef(aws.StringValue(input.TaskDefinition))
	if family == "" || rev == 0 {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	var rec TaskDefRecord
	found, err := getJSON(kv, TaskDefRevKey(family, rev), &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	rec.Status = TaskDefStatusInactive
	if err := putJSON(kv, TaskDefRevKey(family, rev), &rec); err != nil {
		return nil, err
	}
	return &ecs.DeregisterTaskDefinitionOutput{TaskDefinition: rec.toAWS()}, nil
}

// DescribeTaskDefinition resolves "family", "family:rev" or an ARN to a revision.
func (s *Service) DescribeTaskDefinition(_ context.Context, input *ecs.DescribeTaskDefinitionInput, accountID string) (*ecs.DescribeTaskDefinitionOutput, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	rec, err := s.resolveTaskDef(kv, aws.StringValue(input.TaskDefinition))
	if err != nil {
		return nil, err
	}
	return &ecs.DescribeTaskDefinitionOutput{TaskDefinition: rec.toAWS(), Tags: tagsToAWS(rec.Tags)}, nil
}

// ListTaskDefinitions returns revision ARNs, optionally filtered by family and
// by status. Matching AWS, the status defaults to ACTIVE when unset, so
// deregistered (INACTIVE) revisions drop off the default listing.
func (s *Service) ListTaskDefinitions(_ context.Context, input *ecs.ListTaskDefinitionsInput, accountID string) (*ecs.ListTaskDefinitionsOutput, error) {
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	wantStatus := aws.StringValue(input.Status)
	if wantStatus == "" {
		wantStatus = TaskDefStatusActive
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
		if found && rec.Status == wantStatus {
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
	if r.TaskRoleArn != "" {
		td.TaskRoleArn = aws.String(r.TaskRoleArn)
	}
	if r.ExecutionRoleArn != "" {
		td.ExecutionRoleArn = aws.String(r.ExecutionRoleArn)
	}
	for _, c := range r.Containers {
		td.ContainerDefinitions = append(td.ContainerDefinitions, c.toAWS())
	}
	return td
}
