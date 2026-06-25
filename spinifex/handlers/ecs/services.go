package handlers_ecs

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// targetRegistrar registers/deregisters a service task's awsvpc ENI IP with an
// ELBv2 target group. The scheduler is the single writer (ecs-v1.md Q8): it
// registers on the RUNNING transition and deregisters on STOPPED.
type targetRegistrar interface {
	Register(accountID, tgARN, ip string, port int) error
	Deregister(accountID, tgARN, ip string, port int) error
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

func (r *natsTargetRegistrar) Register(accountID, tgARN, ip string, port int) error {
	_, err := handlers_elbv2.NewNATSELBv2Service(r.nc).RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(tgARN),
		Targets:        []*elbv2.TargetDescription{{Id: aws.String(ip), Port: aws.Int64(int64(port))}},
	}, accountID)
	return err
}

func (r *natsTargetRegistrar) Deregister(accountID, tgARN, ip string, port int) error {
	_, err := handlers_elbv2.NewNATSELBv2Service(r.nc).DeregisterTargets(&elbv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String(tgARN),
		Targets:        []*elbv2.TargetDescription{{Id: aws.String(ip), Port: aws.Int64(int64(port))}},
	}, accountID)
	return err
}

// --- Service API ---

// CreateService persists a REPLICA service and reconciles up to desiredCount.
// DAEMON scheduling and serviceRegistries are rejected in v1 (Q15). Re-creating
// an existing ACTIVE service returns the existing record (idempotent).
func (s *Service) CreateService(input *ecs.CreateServiceInput, accountID string) (*ecs.CreateServiceOutput, error) {
	name := aws.StringValue(input.ServiceName)
	if name == "" {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter)
	}
	if strategy := aws.StringValue(input.SchedulingStrategy); strategy != "" && strategy != SchedulingStrategyReplica {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter) // DAEMON unsupported (Q15)
	}
	if len(input.ServiceRegistries) > 0 {
		return nil, errors.New(awserrors.ErrorECSInvalidParameter) // Route53 v0 not landed (Q15)
	}

	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	var clusterRec ClusterRecord
	found, err := getJSON(kv, ClusterMetaKey(cluster), &clusterRec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSClusterNotFound)
	}

	var existing ServiceRecord
	if ok, gerr := getJSON(kv, ServiceKey(cluster, name), &existing); gerr != nil {
		return nil, gerr
	} else if ok && existing.Status == ServiceStatusActive {
		return &ecs.CreateServiceOutput{Service: s.serviceToAWS(accountID, &existing)}, nil
	}

	taskDef, err := s.resolveTaskDef(kv, aws.StringValue(input.TaskDefinition))
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
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if input.NetworkConfiguration != nil && input.NetworkConfiguration.AwsvpcConfiguration != nil {
		v := input.NetworkConfiguration.AwsvpcConfiguration
		rec.Subnets = awsStringSlice(v.Subnets)
		rec.SecurityGroups = awsStringSlice(v.SecurityGroups)
		rec.AssignPublicIP = aws.StringValue(v.AssignPublicIp)
	}
	if err := putJSON(kv, ServiceKey(cluster, name), &rec); err != nil {
		return nil, err
	}

	if err := s.reconcileService(kv, accountID, &rec); err != nil {
		slog.Error("ECS CreateService: initial reconcile failed", "service", name, "err", err)
	}
	return &ecs.CreateServiceOutput{Service: s.serviceToAWS(accountID, &rec)}, nil
}

// UpdateService mutates desiredCount and/or the task definition, then reconciles.
func (s *Service) UpdateService(input *ecs.UpdateServiceInput, accountID string) (*ecs.UpdateServiceOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	name := serviceShortName(aws.StringValue(input.Service))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	var rec ServiceRecord
	found, err := getJSON(kv, ServiceKey(cluster, name), &rec)
	if err != nil {
		return nil, err
	}
	if !found || rec.Status == ServiceStatusInactive {
		return nil, errors.New(awserrors.ErrorECSServiceNotFound)
	}

	if input.DesiredCount != nil {
		rec.DesiredCount = int(aws.Int64Value(input.DesiredCount))
	}
	if td := aws.StringValue(input.TaskDefinition); td != "" {
		taskDef, terr := s.resolveTaskDef(kv, td)
		if terr != nil {
			return nil, terr
		}
		rec.TaskDefFamily = taskDef.Family
		rec.TaskDefRevision = taskDef.Revision
		rec.TaskDefARN = taskDef.ARN
		rec.NetworkMode = resolveNetworkMode(taskDef)
		rec.DeploymentID = uuid.NewString()
	}
	rec.UpdatedAt = time.Now().UTC()
	if err := putJSON(kv, ServiceKey(cluster, name), &rec); err != nil {
		return nil, err
	}
	if err := s.reconcileService(kv, accountID, &rec); err != nil {
		slog.Error("ECS UpdateService: reconcile failed", "service", name, "err", err)
	}
	return &ecs.UpdateServiceOutput{Service: s.serviceToAWS(accountID, &rec)}, nil
}

// DeleteService drains the service to zero, force-stops its tasks, and marks it
// INACTIVE (kept describable, AWS parity).
func (s *Service) DeleteService(input *ecs.DeleteServiceInput, accountID string) (*ecs.DeleteServiceOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	name := serviceShortName(aws.StringValue(input.Service))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	var rec ServiceRecord
	found, err := getJSON(kv, ServiceKey(cluster, name), &rec)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New(awserrors.ErrorECSServiceNotFound)
	}

	rec.DesiredCount = 0
	tasks, err := s.listServiceTasks(kv, cluster, name)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		s.forceStopTask(kv, accountID, &tasks[i], "Service deleted")
	}
	rec.Status = ServiceStatusInactive
	rec.RunningCount = 0
	rec.PendingCount = 0
	rec.UpdatedAt = time.Now().UTC()
	if err := putJSON(kv, ServiceKey(cluster, name), &rec); err != nil {
		return nil, err
	}
	return &ecs.DeleteServiceOutput{Service: s.serviceToAWS(accountID, &rec)}, nil
}

// DescribeServices returns the named services; unknown names surface as failures.
func (s *Service) DescribeServices(input *ecs.DescribeServicesInput, accountID string) (*ecs.DescribeServicesOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	out := &ecs.DescribeServicesOutput{}
	for _, ref := range awsStringSlice(input.Services) {
		name := serviceShortName(ref)
		var rec ServiceRecord
		found, gerr := getJSON(kv, ServiceKey(cluster, name), &rec)
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
func (s *Service) ListServices(input *ecs.ListServicesInput, accountID string) (*ecs.ListServicesOutput, error) {
	cluster := clusterShortName(aws.StringValue(input.Cluster))
	kv, err := s.bucket(accountID)
	if err != nil {
		return nil, err
	}
	recs, err := s.listServiceRecords(kv, cluster)
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

// reconcileService converges a service's running task count on its desired count:
// it launches the shortfall via RunTask (tagged with the service group so the
// task counts toward the service) and force-stops any surplus. Counts on the
// record are refreshed as a side effect.
func (s *Service) reconcileService(kv nats.KeyValue, accountID string, svc *ServiceRecord) error {
	if svc.Status != ServiceStatusActive {
		return nil
	}
	tasks, err := s.listServiceTasks(kv, svc.Cluster, svc.Name)
	if err != nil {
		return err
	}

	running, pending := 0, 0
	for i := range tasks {
		switch tasks[i].LastStatus {
		case TaskStatusRunning:
			running++
		case TaskStatusPending:
			pending++
		}
	}

	active := running + pending
	switch {
	case active < svc.DesiredCount:
		s.launchServiceTasks(accountID, svc, svc.DesiredCount-active)
	case active > svc.DesiredCount:
		s.stopServiceSurplus(kv, accountID, tasks, active-svc.DesiredCount)
	}

	// Recount after launch/stop so the persisted counts reflect current state.
	svc.RunningCount, svc.PendingCount = s.countServiceTasks(kv, svc.Cluster, svc.Name)
	return putJSON(kv, ServiceKey(svc.Cluster, svc.Name), svc)
}

// countServiceTasks returns a service's current RUNNING and PENDING task counts.
func (s *Service) countServiceTasks(kv nats.KeyValue, cluster, name string) (running, pending int) {
	tasks, err := s.listServiceTasks(kv, cluster, name)
	if err != nil {
		return 0, 0
	}
	for i := range tasks {
		switch tasks[i].LastStatus {
		case TaskStatusRunning:
			running++
		case TaskStatusPending:
			pending++
		}
	}
	return running, pending
}

// reconcileAllServices is the scheduler-tick fan-out: every ACTIVE service in
// every ECS account bucket is reconciled. Runs only on the scheduler leader.
func (s *Service) reconcileAllServices() {
	js, err := s.js()
	if err != nil {
		return
	}
	for bucket := range js.KeyValueStoreNames() {
		accountID, ok := accountIDFromBucket(bucket)
		if !ok {
			continue
		}
		kv, err := js.KeyValue(bucket)
		if err != nil {
			continue
		}
		recs, err := s.allServiceRecords(kv)
		if err != nil {
			continue
		}
		for i := range recs {
			if err := s.reconcileService(kv, accountID, &recs[i]); err != nil {
				slog.Error("ECS reconcile: service failed", "service", recs[i].Name, "err", err)
			}
		}
	}
}

// launchServiceTasks places n replacement tasks via the standard RunTask flow,
// tagged with the service group + deployment so they count toward the service.
func (s *Service) launchServiceTasks(accountID string, svc *ServiceRecord, n int) {
	in := &ecs.RunTaskInput{
		Cluster:        aws.String(svc.Cluster),
		TaskDefinition: aws.String(svc.TaskDefARN),
		Count:          aws.Int64(int64(n)),
		Group:          aws.String(serviceTaskGroup(svc.Name)),
		StartedBy:      aws.String("ecs-svc/" + svc.DeploymentID),
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
	out, err := s.RunTask(in, accountID)
	if err != nil {
		slog.Error("ECS reconcile: launch failed", "service", svc.Name, "err", err)
		return
	}
	if len(out.Failures) > 0 {
		slog.Warn("ECS reconcile: launch had placement failures", "service", svc.Name, "failures", len(out.Failures))
	}
}

// stopServiceSurplus force-stops n of a service's tasks, preferring PENDING tasks
// (least disruptive) before RUNNING ones.
func (s *Service) stopServiceSurplus(kv nats.KeyValue, accountID string, tasks []TaskRecord, n int) {
	order := make([]int, 0, len(tasks))
	for i := range tasks {
		if tasks[i].LastStatus == TaskStatusPending {
			order = append(order, i)
		}
	}
	for i := range tasks {
		if tasks[i].LastStatus == TaskStatusRunning {
			order = append(order, i)
		}
	}
	for _, idx := range order {
		if n <= 0 {
			break
		}
		s.forceStopTask(kv, accountID, &tasks[idx], "Service scaled in")
		n--
	}
}

// --- Helpers ---

// listServiceTasks returns a cluster's non-STOPPED tasks owned by the service.
func (s *Service) listServiceTasks(kv nats.KeyValue, cluster, name string) ([]TaskRecord, error) {
	group := serviceTaskGroup(name)
	all, err := s.listTaskRecords(kv, cluster)
	if err != nil {
		return nil, err
	}
	out := make([]TaskRecord, 0, len(all))
	for _, t := range all {
		if t.Group == group && t.LastStatus != TaskStatusStopped {
			out = append(out, t)
		}
	}
	return out, nil
}

// listTaskRecords returns every task record in a cluster.
func (s *Service) listTaskRecords(kv nats.KeyValue, cluster string) ([]TaskRecord, error) {
	keys, err := keysWithPrefix(kv, TasksPrefix(cluster))
	if err != nil {
		return nil, err
	}
	out := make([]TaskRecord, 0, len(keys))
	for _, k := range keys {
		var rec TaskRecord
		found, err := getJSON(kv, k, &rec)
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
func (s *Service) listServiceRecords(kv nats.KeyValue, cluster string) ([]ServiceRecord, error) {
	keys, err := keysWithPrefix(kv, ServicesPrefix(cluster))
	if err != nil {
		return nil, err
	}
	out := make([]ServiceRecord, 0, len(keys))
	for _, k := range keys {
		var rec ServiceRecord
		found, err := getJSON(kv, k, &rec)
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
func (s *Service) allServiceRecords(kv nats.KeyValue) ([]ServiceRecord, error) {
	keys, err := keysWithPrefix(kv, "clusters/")
	if err != nil {
		return nil, err
	}
	out := make([]ServiceRecord, 0)
	for _, k := range keys {
		if !strings.Contains(k, "/services/") {
			continue
		}
		var rec ServiceRecord
		found, err := getJSON(kv, k, &rec)
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
	return svc
}
