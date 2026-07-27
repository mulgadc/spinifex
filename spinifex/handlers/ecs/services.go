package handlers_ecs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// targetRegistrar registers/deregisters a service task's awsvpc ENI IP with an
// ELBv2 target group. The scheduler is the single writer (ecs-v1.md Q8): it
// registers on the RUNNING transition and deregisters on STOPPED.
type targetRegistrar interface {
	Register(ctx context.Context, accountID, tgARN, ip string, port int) error
	Deregister(ctx context.Context, accountID, tgARN, ip string, port int) error
}

// natsTargetRegistrar drives the existing elbv2.RegisterTargets/DeregisterTargets
// NATS handlers; no new ELBv2 surface is added for ECS (Q16).
type natsTargetRegistrar struct {
	nc *nats.Conn
}

var _ targetRegistrar = (*natsTargetRegistrar)(nil)

func newNATSTargetRegistrar(nc *nats.Conn) *natsTargetRegistrar {
	return &natsTargetRegistrar{nc: nc}
}

func (r *natsTargetRegistrar) Register(ctx context.Context, accountID, tgARN, ip string, port int) error {
	_, err := handlers_elbv2.NewNATSELBv2Service(r.nc).RegisterTargets(ctx, &elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(tgARN),
		Targets:        []*elbv2.TargetDescription{{Id: aws.String(ip), Port: aws.Int64(int64(port))}},
	}, accountID)
	return err
}

func (r *natsTargetRegistrar) Deregister(ctx context.Context, accountID, tgARN, ip string, port int) error {
	_, err := handlers_elbv2.NewNATSELBv2Service(r.nc).DeregisterTargets(ctx, &elbv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String(tgARN),
		Targets:        []*elbv2.TargetDescription{{Id: aws.String(ip), Port: aws.Int64(int64(port))}},
	}, accountID)
	return err
}

// eipManager gives an awsvpc task a public endpoint by allocating an Elastic IP
// and associating it with the task ENI, and releases it on STOPPED. The
// scheduler is the single writer, mirroring targetRegistrar.
type eipManager interface {
	AllocateAndAssociate(ctx context.Context, accountID, eniID string) (publicIP, allocationID string, err error)
	Release(ctx context.Context, accountID, allocationID string) error
}

// natsEIPManager drives the existing ec2 EIP NATS handlers; no new surface is
// added for ECS.
type natsEIPManager struct {
	nc *nats.Conn
}

var _ eipManager = (*natsEIPManager)(nil)

func newNATSEIPManager(nc *nats.Conn) *natsEIPManager {
	return &natsEIPManager{nc: nc}
}

func (m *natsEIPManager) AllocateAndAssociate(ctx context.Context, accountID, eniID string) (string, string, error) {
	svc := handlers_ec2_eip.NewNATSEIPService(m.nc)
	alloc, err := svc.AllocateAddress(ctx, &ec2.AllocateAddressInput{Domain: aws.String("vpc")}, accountID)
	if err != nil {
		return "", "", err
	}
	publicIP, allocationID := aws.StringValue(alloc.PublicIp), aws.StringValue(alloc.AllocationId)
	if _, err = svc.AssociateAddress(ctx, &ec2.AssociateAddressInput{
		AllocationId:       alloc.AllocationId,
		NetworkInterfaceId: aws.String(eniID),
	}, accountID); err != nil {
		// Release the orphaned allocation so a failed associate leaks nothing.
		if _, rerr := svc.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: alloc.AllocationId}, accountID); rerr != nil {
			slog.ErrorContext(ctx, "ECS auto-EIP: release after failed associate", "alloc", allocationID, "err", rerr)
		}
		return "", "", err
	}
	return publicIP, allocationID, nil
}

func (m *natsEIPManager) Release(ctx context.Context, accountID, allocationID string) error {
	svc := handlers_ec2_eip.NewNATSEIPService(m.nc)
	// Disassociate first (VPC EIPs cannot be released while associated); the
	// association ID is resolved from the allocation.
	desc, err := svc.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		AllocationIds: []*string{aws.String(allocationID)},
	}, accountID)
	if err == nil {
		for _, addr := range desc.Addresses {
			if addr.AssociationId != nil && *addr.AssociationId != "" {
				if _, derr := svc.DisassociateAddress(ctx, &ec2.DisassociateAddressInput{
					AssociationId: addr.AssociationId,
				}, accountID); derr != nil {
					slog.ErrorContext(ctx, "ECS auto-EIP: disassociate failed", "alloc", allocationID, "err", derr)
				}
			}
		}
	}
	_, err = svc.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{AllocationId: aws.String(allocationID)}, accountID)
	return err
}

// --- Service API ---

// CreateService persists a REPLICA service and reconciles up to desiredCount.
// DAEMON scheduling and serviceRegistries are rejected in v1 (Q15). Re-creating
// an existing ACTIVE service returns the existing record (idempotent).
func (s *Service) CreateService(ctx context.Context, input *ecs.CreateServiceInput, accountID string) (*ecs.CreateServiceOutput, error) {
	name := aws.StringValue(input.ServiceName)
	if name == "" {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	if strategy := aws.StringValue(input.SchedulingStrategy); strategy != "" && strategy != SchedulingStrategyReplica {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter) // DAEMON unsupported (Q15)
	}
	// Service discovery needs SRV records, which the DNS writer does not yet emit,
	// and a servicediscovery surface to resolve the registry ARN against (Q15).
	if len(input.ServiceRegistries) > 0 {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}

	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var clusterRec ClusterRecord
	found, err := getJSON(ctx, kv, ClusterMetaKey(cluster), &clusterRec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSClusterNotFound)
	}

	var existing ServiceRecord
	if ok, gerr := getJSON(ctx, kv, ServiceKey(cluster, name), &existing); gerr != nil {
		return nil, gerr
	} else if ok && existing.Status == ServiceStatusActive {
		return &ecs.CreateServiceOutput{Service: s.serviceToAWS(accountID, &existing)}, nil
	}

	taskDef, err := s.resolveTaskDef(ctx, kv, aws.StringValue(input.TaskDefinition))
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	rec := ServiceRecord{
		Name:               name,
		ARN:                ServiceARN(s.region, accountID, cluster, name),
		Cluster:            cluster,
		TaskDefFamily:      taskDef.Family,
		TaskDefRevision:    taskDef.Revision,
		TaskDefARN:         taskDef.ARN,
		DesiredCount:       int(aws.Int64Value(input.DesiredCount)),
		Status:             ServiceStatusActive,
		SchedulingStrategy: SchedulingStrategyReplica,
		LaunchType:         aws.StringValue(input.LaunchType),
		NetworkMode:        resolveNetworkMode(taskDef),
		PlacementStrategy:  placementStrategyFromAWS(input.PlacementStrategy),
		LoadBalancers:      loadBalancersFromAWS(input.LoadBalancers),
		DeploymentID:       uuid.NewString(),
		Tags:               tagsToMap(input.Tags),
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if input.NetworkConfiguration != nil && input.NetworkConfiguration.AwsvpcConfiguration != nil {
		v := input.NetworkConfiguration.AwsvpcConfiguration
		rec.Subnets = awsStringSlice(v.Subnets)
		rec.SecurityGroups = awsStringSlice(v.SecurityGroups)
		rec.AssignPublicIP = aws.StringValue(v.AssignPublicIp)
	}
	applyDeploymentConfig(&rec, input.DeploymentConfiguration)
	rec.LastGoodTaskDefARN = taskDef.ARN
	rec.Deployments = []Deployment{newPrimaryDeployment(rec.DeploymentID, taskDef, rec.DesiredCount)}
	if err := putJSON(ctx, kv, ServiceKey(cluster, name), &rec); err != nil {
		return nil, err
	}

	if err := s.reconcileService(ctx, kv, accountID, &rec); err != nil {
		slog.ErrorContext(ctx, "ECS CreateService: initial reconcile failed", "service", name, "err", err)
	}
	return &ecs.CreateServiceOutput{Service: s.serviceToAWS(accountID, &rec)}, nil
}

// UpdateService mutates desiredCount and/or the task definition, then reconciles.
func (s *Service) UpdateService(ctx context.Context, input *ecs.UpdateServiceInput, accountID string) (*ecs.UpdateServiceOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	name := serviceShortName(aws.StringValue(input.Service))
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var rec ServiceRecord
	found, err := getJSON(ctx, kv, ServiceKey(cluster, name), &rec)
	if err != nil {
		return nil, err
	}
	if !found || rec.Status == ServiceStatusInactive {
		return nil, errors.New(awserrors.ErrorECSServiceNotFound)
	}

	rec.ensurePrimaryDeployment()
	if input.DeploymentConfiguration != nil {
		applyDeploymentConfig(&rec, input.DeploymentConfiguration)
	}
	if input.DesiredCount != nil {
		rec.DesiredCount = int(aws.Int64Value(input.DesiredCount))
	}
	if td := aws.StringValue(input.TaskDefinition); td != "" {
		taskDef, terr := s.resolveTaskDef(ctx, kv, td)
		if terr != nil {
			return nil, terr
		}
		// A new task definition starts a rolling deployment only when it differs
		// from the current PRIMARY; a no-op re-apply keeps the deployment steady.
		if primary := rec.primaryDeployment(); primary == nil || primary.TaskDefARN != taskDef.ARN {
			rec.startDeployment(taskDef)
		}
	}
	rec.UpdatedAt = time.Now().UTC()
	if err := putJSON(ctx, kv, ServiceKey(cluster, name), &rec); err != nil {
		return nil, err
	}
	if err := s.reconcileService(ctx, kv, accountID, &rec); err != nil {
		slog.ErrorContext(ctx, "ECS UpdateService: reconcile failed", "service", name, "err", err)
	}
	return &ecs.UpdateServiceOutput{Service: s.serviceToAWS(accountID, &rec)}, nil
}

// DeleteService drains the service to zero, force-stops its tasks, and marks it
// INACTIVE (kept describable, AWS parity).
func (s *Service) DeleteService(ctx context.Context, input *ecs.DeleteServiceInput, accountID string) (*ecs.DeleteServiceOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	name := serviceShortName(aws.StringValue(input.Service))
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var rec ServiceRecord
	found, err := getJSON(ctx, kv, ServiceKey(cluster, name), &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSServiceNotFound)
	}

	rec.DesiredCount = 0
	tasks, err := s.listServiceTasks(ctx, kv, cluster, name)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		s.requestStopTask(ctx, kv, accountID, &tasks[i], "Service deleted")
	}
	rec.Status = ServiceStatusInactive
	rec.RunningCount = 0
	rec.PendingCount = 0
	rec.UpdatedAt = time.Now().UTC()
	if err := putJSON(ctx, kv, ServiceKey(cluster, name), &rec); err != nil {
		return nil, err
	}
	return &ecs.DeleteServiceOutput{Service: s.serviceToAWS(accountID, &rec)}, nil
}

// DescribeServices returns the named services; unknown names surface as failures.
func (s *Service) DescribeServices(ctx context.Context, input *ecs.DescribeServicesInput, accountID string) (*ecs.DescribeServicesOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := &ecs.DescribeServicesOutput{}
	for _, ref := range awsStringSlice(input.Services) {
		name := serviceShortName(ref)
		var rec ServiceRecord
		found, gerr := getJSON(ctx, kv, ServiceKey(cluster, name), &rec)
		if gerr != nil {
			return nil, gerr
		}
		if found {
			out.Services = append(out.Services, s.serviceToAWS(accountID, &rec))
		} else {
			out.Failures = append(out.Failures, &ecs.Failure{Arn: aws.String(ref), Reason: aws.String("MISSING")})
		}
	}
	return out, nil
}

// ListServices returns the ARNs of every service in a cluster.
func (s *Service) ListServices(ctx context.Context, input *ecs.ListServicesInput, accountID string) (*ecs.ListServicesOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return nil, err
	}
	recs, err := s.listServiceRecords(ctx, kv, cluster)
	if err != nil {
		return nil, err
	}
	out := &ecs.ListServicesOutput{}
	for i := range recs {
		out.ServiceArns = append(out.ServiceArns, aws.String(recs[i].ARN))
	}
	return out, nil
}

// --- Reconciliation ---

// reconcileService drives a service's rolling deployment toward its desired
// count: it launches PRIMARY-deployment tasks within the maximumPercent ceiling
// and drains superseded (old) tasks while healthy running stays at/above
// minimumHealthyPercent, then advances the rollout / circuit-breaker state. Per-
// deployment and overall counts are refreshed as a side effect.
func (s *Service) reconcileService(ctx context.Context, kv jetstream.KeyValue, accountID string, svc *ServiceRecord) error {
	if svc.Status != ServiceStatusActive {
		return nil
	}
	svc.normalizeDeploymentConfig()
	svc.ensurePrimaryDeployment()

	tasks, err := s.listServiceTasks(ctx, kv, svc.Cluster, svc.Name)
	if err != nil {
		return err
	}
	running, pending := tallyDeployments(svc, tasks)

	primary := svc.primaryDeployment()
	if primary != nil {
		// A failing deployment trips the breaker (and may roll back); persist and
		// let the next tick roll the replacement in.
		if tripCircuitBreaker(svc, primary) {
			svc.RunningCount, svc.PendingCount = running, pending
			return putJSON(ctx, kv, ServiceKey(svc.Cluster, svc.Name), svc)
		}
		desired := svc.DesiredCount
		maxCount := desired * svc.MaximumPercent / 100
		minCount := ceilPercent(desired, svc.MinimumHealthyPercent)

		primaryActive := primary.RunningCount + primary.PendingCount
		if primaryActive < desired && primary.RolloutState != RolloutStateFailed {
			n := min(desired-primaryActive, max(maxCount-(running+pending), 0))
			if n > 0 {
				s.launchDeploymentTasks(ctx, accountID, svc, primary, n)
			}
		}
		s.stopSurplusTasks(ctx, kv, accountID, tasks, primary.ID, desired, running, minCount)

		// Re-tally after launch/stop so rollout state + persisted counts are current.
		if fresh, ferr := s.listServiceTasks(ctx, kv, svc.Cluster, svc.Name); ferr == nil {
			running, pending = tallyDeployments(svc, fresh)
		}
		updateRolloutState(svc, primary, desired)
	}

	svc.RunningCount, svc.PendingCount = running, pending
	return putJSON(ctx, kv, ServiceKey(svc.Cluster, svc.Name), svc)
}

// reconcileAllServices is the scheduler-tick fan-out: every ACTIVE service in
// every ECS account bucket is reconciled. Runs only on the scheduler leader.
// Returns an error when the account enumeration could not be completed, so a
// pass that could not see the whole fleet is reported rather than read as "no
// services to reconcile" — every unlisted account stalls below its desired count.
func (s *Service) reconcileAllServices(ctx context.Context) error {
	js, err := s.js()
	if err != nil {
		return err
	}
	buckets, err := accountBuckets(ctx, s.nc)
	if err != nil {
		return err
	}
	for _, bucket := range buckets {
		kv, err := js.KeyValue(ctx, bucket.name)
		if err != nil {
			slog.Error("ECS reconcile: open bucket failed", "bucket", bucket.name, "err", err)
			continue
		}
		recs, err := s.allServiceRecords(ctx, kv)
		if err != nil {
			slog.Error("ECS reconcile: list services failed", "bucket", bucket.name, "err", err)
			continue
		}
		for i := range recs {
			if err := s.reconcileService(ctx, kv, bucket.accountID, &recs[i]); err != nil {
				slog.Error("ECS reconcile: service failed", "service", recs[i].Name, "err", err)
			}
		}
	}
	return nil
}

// launchDeploymentTasks places n tasks for a specific deployment via the standard
// RunTask flow, tagged with the service group + the deployment ID (StartedBy) so
// the reconciler maps them back to the deployment they belong to.
func (s *Service) launchDeploymentTasks(ctx context.Context, accountID string, svc *ServiceRecord, dep *Deployment, n int) {
	in := &ecs.RunTaskInput{
		Cluster:        aws.String(svc.Cluster),
		TaskDefinition: aws.String(dep.TaskDefARN),
		Count:          aws.Int64(int64(n)),
		Group:          aws.String(serviceTaskGroup(svc.Name)),
		StartedBy:      aws.String(deploymentStartedBy(dep.ID)),
	}
	if svc.PlacementStrategy != "" {
		in.PlacementStrategy = []*ecs.PlacementStrategy{{Type: aws.String(svc.PlacementStrategy)}}
	}
	if svc.NetworkMode == NetworkModeAwsvpc {
		in.NetworkConfiguration = &ecs.NetworkConfiguration{
			AwsvpcConfiguration: &ecs.AwsVpcConfiguration{
				Subnets:        aws.StringSlice(svc.Subnets),
				SecurityGroups: aws.StringSlice(svc.SecurityGroups),
			},
		}
		if svc.AssignPublicIP != "" {
			in.NetworkConfiguration.AwsvpcConfiguration.AssignPublicIp = aws.String(svc.AssignPublicIP)
		}
	}
	out, err := s.RunTask(ctx, in, accountID)
	if err != nil {
		slog.ErrorContext(ctx, "ECS reconcile: launch failed", "service", svc.Name, "err", err)
		return
	}
	if len(out.Failures) > 0 {
		slog.WarnContext(ctx, "ECS reconcile: launch had placement failures", "service", svc.Name, "failures", len(out.Failures))
	}
}

// stopSurplusTasks drains a service's excess tasks: superseded (non-primary)
// deployment tasks toward zero, and the primary's own surplus on a scale-in.
// PENDING surplus is stopped freely; RUNNING surplus is gated so healthy running
// never drops below minimumHealthyPercent (minCount).
func (s *Service) stopSurplusTasks(ctx context.Context, kv jetstream.KeyValue, accountID string, tasks []TaskRecord, primaryID string, desired, runningTotal, minCount int) {
	var oldPending, oldRunning, primaryPending, primaryRunning []int
	for i := range tasks {
		isPrimary := deploymentIDFromStartedBy(tasks[i].StartedBy) == primaryID
		switch tasks[i].LastStatus {
		case TaskStatusPending:
			if isPrimary {
				primaryPending = append(primaryPending, i)
			} else {
				oldPending = append(oldPending, i)
			}
		case TaskStatusRunning:
			if isPrimary {
				primaryRunning = append(primaryRunning, i)
			} else {
				oldRunning = append(oldRunning, i)
			}
		}
	}
	primarySurplus := max(len(primaryPending)+len(primaryRunning)-desired, 0)
	runBudget := max(runningTotal-minCount, 0)
	// Cooperative stop: the agent reaps the container, then recordTaskState performs
	// the single capacity release. A never-placed task force-stops internally.
	stop := func(i int, reason string) { s.requestStopTask(ctx, kv, accountID, &tasks[i], reason) }

	for _, i := range oldPending {
		stop(i, deploymentSupersededReason)
	}
	for _, i := range primaryPending {
		if primarySurplus <= 0 {
			break
		}
		stop(i, "Service scaled in")
		primarySurplus--
	}
	for _, i := range oldRunning {
		if runBudget <= 0 {
			break
		}
		stop(i, deploymentSupersededReason)
		runBudget--
	}
	for _, i := range primaryRunning {
		if primarySurplus <= 0 || runBudget <= 0 {
			break
		}
		stop(i, "Service scaled in")
		primarySurplus--
		runBudget--
	}
}

// --- Helpers ---

// listServiceTasks returns a cluster's live tasks owned by the service: neither
// STOPPED nor already requested to stop (DesiredStatus=STOPPED). A cooperatively
// stopped task lingers RUNNING until the agent reaps it, but it is on its way out,
// so it must not count toward the service's running total for scaling decisions.
func (s *Service) listServiceTasks(ctx context.Context, kv jetstream.KeyValue, cluster, name string) ([]TaskRecord, error) {
	group := serviceTaskGroup(name)
	all, err := s.listTaskRecords(ctx, kv, cluster)
	if err != nil {
		return nil, err
	}
	out := make([]TaskRecord, 0, len(all))
	for _, t := range all {
		if t.Group == group && t.LastStatus != TaskStatusStopped && t.DesiredStatus != TaskStatusStopped {
			out = append(out, t)
		}
	}
	return out, nil
}

// listTaskRecords returns every task record in a cluster.
func (s *Service) listTaskRecords(ctx context.Context, kv jetstream.KeyValue, cluster string) ([]TaskRecord, error) {
	keys, err := keysWithPrefix(ctx, kv, TasksPrefix(cluster))
	if err != nil {
		return nil, err
	}
	out := make([]TaskRecord, 0, len(keys))
	for _, k := range keys {
		var rec TaskRecord
		found, err := getJSON(ctx, kv, k, &rec)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, rec)
		}
	}
	return out, nil
}

// listServiceRecords returns every service record in a cluster.
func (s *Service) listServiceRecords(ctx context.Context, kv jetstream.KeyValue, cluster string) ([]ServiceRecord, error) {
	keys, err := keysWithPrefix(ctx, kv, ServicesPrefix(cluster))
	if err != nil {
		return nil, err
	}
	out := make([]ServiceRecord, 0, len(keys))
	for _, k := range keys {
		var rec ServiceRecord
		found, err := getJSON(ctx, kv, k, &rec)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, rec)
		}
	}
	return out, nil
}

// allServiceRecords returns every service record across all clusters in a bucket.
func (s *Service) allServiceRecords(ctx context.Context, kv jetstream.KeyValue) ([]ServiceRecord, error) {
	keys, err := keysWithPrefix(ctx, kv, "clusters/")
	if err != nil {
		return nil, err
	}
	out := make([]ServiceRecord, 0)
	for _, k := range keys {
		if !strings.Contains(k, "/services/") {
			continue
		}
		var rec ServiceRecord
		found, err := getJSON(ctx, kv, k, &rec)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, rec)
		}
	}
	return out, nil
}

// serviceShortName extracts the service name from a name or a service ARN.
func serviceShortName(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.LastIndexByte(ref, '/'); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// loadBalancersFromAWS maps the SDK loadBalancer list to the persisted subset.
func loadBalancersFromAWS(in []*ecs.LoadBalancer) []LoadBalancerTarget {
	out := make([]LoadBalancerTarget, 0, len(in))
	for _, lb := range in {
		if lb == nil || aws.StringValue(lb.TargetGroupArn) == "" {
			continue
		}
		out = append(out, LoadBalancerTarget{
			TargetGroupARN: aws.StringValue(lb.TargetGroupArn),
			ContainerName:  aws.StringValue(lb.ContainerName),
			ContainerPort:  int(aws.Int64Value(lb.ContainerPort)),
		})
	}
	return out
}

func (s *Service) serviceToAWS(accountID string, r *ServiceRecord) *ecs.Service {
	svc := &ecs.Service{
		ServiceName:        aws.String(r.Name),
		ServiceArn:         aws.String(r.ARN),
		ClusterArn:         aws.String(ClusterARN(s.region, accountID, r.Cluster)),
		Status:             aws.String(r.Status),
		DesiredCount:       aws.Int64(int64(r.DesiredCount)),
		RunningCount:       aws.Int64(int64(r.RunningCount)),
		PendingCount:       aws.Int64(int64(r.PendingCount)),
		SchedulingStrategy: aws.String(r.SchedulingStrategy),
		TaskDefinition:     aws.String(r.TaskDefARN),
		Tags:               tagsToAWS(r.Tags),
	}
	if r.LaunchType != "" {
		svc.LaunchType = aws.String(r.LaunchType)
	}
	for _, lb := range r.LoadBalancers {
		svc.LoadBalancers = append(svc.LoadBalancers, &ecs.LoadBalancer{
			TargetGroupArn: aws.String(lb.TargetGroupARN),
			ContainerName:  aws.String(lb.ContainerName),
			ContainerPort:  aws.Int64(int64(lb.ContainerPort)),
		})
	}
	if r.MinimumHealthyPercent > 0 || r.MaximumPercent > 0 {
		svc.DeploymentConfiguration = &ecs.DeploymentConfiguration{
			MinimumHealthyPercent: aws.Int64(int64(r.MinimumHealthyPercent)),
			MaximumPercent:        aws.Int64(int64(r.MaximumPercent)),
			DeploymentCircuitBreaker: &ecs.DeploymentCircuitBreaker{
				Enable:   aws.Bool(r.CircuitBreakerEnable),
				Rollback: aws.Bool(r.CircuitBreakerRollback),
			},
		}
	}
	for i := range r.Deployments {
		d := &r.Deployments[i]
		svc.Deployments = append(svc.Deployments, &ecs.Deployment{
			Id:             aws.String(d.ID),
			Status:         aws.String(d.Status),
			TaskDefinition: aws.String(d.TaskDefARN),
			DesiredCount:   aws.Int64(int64(d.DesiredCount)),
			RunningCount:   aws.Int64(int64(d.RunningCount)),
			PendingCount:   aws.Int64(int64(d.PendingCount)),
			FailedTasks:    aws.Int64(int64(d.FailedTasks)),
			RolloutState:   aws.String(d.RolloutState),
			RolloutStateReason: func() *string {
				if d.RolloutReason == "" {
					return nil
				}
				return aws.String(d.RolloutReason)
			}(),
		})
	}
	return svc
}
