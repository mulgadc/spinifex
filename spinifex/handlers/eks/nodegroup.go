package handlers_eks

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/google/uuid"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
)

// defaultNodegroupInstanceType mirrors the AWS EKS managed-nodegroup default
// when the caller omits instanceTypes.
const defaultNodegroupInstanceType = "t3.medium"

// defaultNodegroupReadyTimeout / defaultNodegroupReadyPoll bound how long
// launchNodegroupInfra waits for its workers to register Ready (observed via the
// CP state report's Ready-node count, refreshed at the reconcile cadence) before
// marking the nodegroup CREATE_FAILED.
const (
	defaultNodegroupReadyTimeout = 10 * time.Minute
	defaultNodegroupReadyPoll    = 15 * time.Second
)

// ErrNodegroupNotFound is returned by GetNodegroupRecord when the record key is
// absent. Callers translate to the AWS shape (ResourceNotFoundException) at the
// service boundary.
var ErrNodegroupNotFound = errors.New("eks: nodegroup not found")

// NodegroupARN composes a nodegroup ARN matching the AWS shape
// (.../nodegroup/{cluster}/{ng}/{uuid}). The trailing UUID is the per-nodegroup
// discriminator AWS appends; it is generated once at create time and persisted.
func NodegroupARN(region, accountID, cluster, ng, id string) string {
	return fmt.Sprintf("arn:aws:eks:%s:%s:nodegroup/%s/%s/%s", region, accountID, cluster, ng, id)
}

// PutNodegroupRecord writes the record unconditionally.
func PutNodegroupRecord(kv nats.KeyValue, rec *NodegroupRecord) error {
	if rec == nil {
		return errors.New("eks: PutNodegroupRecord nil record")
	}
	if rec.ClusterName == "" || rec.Name == "" {
		return errors.New("eks: PutNodegroupRecord missing cluster or name")
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal nodegroup %s: %w", rec.Name, err)
	}
	key := NodegroupKey(rec.ClusterName, rec.Name)
	if _, err := kv.Put(key, data); err != nil {
		return fmt.Errorf("kv put %s: %w", key, err)
	}
	return nil
}

// ClaimNodegroupRecord atomically creates the nodegroup record, returning
// owned=false when a record already exists. This is the idempotency barrier for
// CreateNodegroup — a duplicate request loses the claim and never launches
// workers. Owner updates after the claim use PutNodegroupRecord.
func ClaimNodegroupRecord(kv nats.KeyValue, rec *NodegroupRecord) (bool, error) {
	if rec == nil {
		return false, errors.New("eks: ClaimNodegroupRecord nil record")
	}
	if rec.ClusterName == "" || rec.Name == "" {
		return false, errors.New("eks: ClaimNodegroupRecord missing cluster or name")
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return false, fmt.Errorf("marshal nodegroup %s: %w", rec.Name, err)
	}
	owned, _, _, err := claimKey(kv, NodegroupKey(rec.ClusterName, rec.Name), data)
	return owned, err
}

// GetNodegroupRecord reads one record. Returns ErrNodegroupNotFound if absent.
func GetNodegroupRecord(kv nats.KeyValue, cluster, ng string) (*NodegroupRecord, error) {
	if cluster == "" || ng == "" {
		return nil, errors.New("eks: GetNodegroupRecord empty cluster or name")
	}
	entry, err := kv.Get(NodegroupKey(cluster, ng))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil, ErrNodegroupNotFound
		}
		return nil, fmt.Errorf("kv get nodegroup: %w", err)
	}
	var rec NodegroupRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal nodegroup %s: %w", ng, err)
	}
	return &rec, nil
}

// ListNodegroupRecords returns every nodegroup record under a cluster, sorted
// by name for stable output.
func ListNodegroupRecords(kv nats.KeyValue, cluster string) ([]*NodegroupRecord, error) {
	if cluster == "" {
		return nil, errors.New("eks: ListNodegroupRecords empty cluster")
	}
	keys, err := kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("kv keys: %w", err)
	}
	prefix := NodegroupsPrefix(cluster)
	out := make([]*NodegroupRecord, 0)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		entry, err := kv.Get(k)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("kv get %s: %w", k, err)
		}
		var rec NodegroupRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return nil, fmt.Errorf("unmarshal nodegroup %s: %w", k, err)
		}
		out = append(out, &rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteNodegroupRecord removes one record. A missing key is a no-op so
// DeleteNodegroup stays idempotent.
func DeleteNodegroupRecord(kv nats.KeyValue, cluster, ng string) error {
	key := NodegroupKey(cluster, ng)
	if err := kv.Delete(key); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	return nil
}

// nodegroupAcctKV opens the per-account bucket for nodegroup handlers.
func (s *EKSServiceImpl) nodegroupAcctKV(accountID string) (nats.KeyValue, error) {
	js, err := s.deps.NATSConn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	acctKV, err := GetOrCreateAccountBucket(js, accountID)
	if err != nil {
		return nil, fmt.Errorf("get account bucket: %w", err)
	}
	return acctKV, nil
}

func (s *EKSServiceImpl) createNodegroup(acctKV nats.KeyValue, input *eks.CreateNodegroupInput, accountID string) (*eks.CreateNodegroupOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	ng := aws.StringValue(input.NodegroupName)
	if cluster == "" || ng == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	subnets := aws.StringValueSlice(input.Subnets)
	if len(subnets) == 0 {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	meta, err := GetClusterMeta(acctKV, cluster)
	if err != nil {
		if errors.Is(err, ErrClusterNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	if meta.Status != ClusterStatusActive {
		slog.Warn("createNodegroup: cluster not ACTIVE", "cluster", cluster, "status", meta.Status)
		return nil, errors.New(awserrors.ErrorInvalidRequest)
	}
	if meta.ControlPlaneENIIP == "" {
		slog.Error("createNodegroup: cluster has no control-plane ENI IP", "cluster", cluster)
		return nil, errors.New(awserrors.ErrorInvalidRequest)
	}
	if meta.Endpoint == "" {
		slog.Error("createNodegroup: cluster has no published endpoint", "cluster", cluster)
		return nil, errors.New(awserrors.ErrorInvalidRequest)
	}
	if meta.ResourcesVpcConfig == nil || meta.ResourcesVpcConfig.VpcId == "" {
		slog.Error("createNodegroup: cluster has no VPC", "cluster", cluster)
		return nil, errors.New(awserrors.ErrorInvalidRequest)
	}

	if _, err := GetNodegroupRecord(acctKV, cluster, ng); err == nil {
		return nil, errors.New(awserrors.ErrorEKSResourceInUse)
	} else if !errors.Is(err, ErrNodegroupNotFound) {
		return nil, err
	}

	instanceTypes := aws.StringValueSlice(input.InstanceTypes)
	if len(instanceTypes) == 0 {
		instanceTypes = []string{defaultNodegroupInstanceType}
	}
	minSize, maxSize, desired := scalingFromInput(input.ScalingConfig)

	amiType := aws.StringValue(input.AmiType)
	if amiType == "" {
		amiType = eks.AMITypesAl2X8664
	}
	version := aws.StringValue(input.Version)
	if version == "" {
		version = meta.Version
	}

	now := time.Now().UTC()
	rec := &NodegroupRecord{
		ClusterName:    cluster,
		Name:           ng,
		Arn:            NodegroupARN(s.deps.Region, accountID, cluster, ng, uuid.NewString()),
		Status:         eks.NodegroupStatusCreating,
		Subnets:        subnets,
		InstanceTypes:  instanceTypes,
		AMIType:        amiType,
		DiskSize:       aws.Int64Value(input.DiskSize),
		ScalingMin:     minSize,
		ScalingMax:     maxSize,
		ScalingDesired: desired,
		Version:        version,
		NodeRole:       aws.StringValue(input.NodeRole),
		Labels:         aws.StringValueMap(input.Labels),
		CreatedAt:      now,
		ModifiedAt:     now,
	}

	// Atomically claim the record; concurrent/duplicate CreateNodegroup requests lose here.
	owned, err := ClaimNodegroupRecord(acctKV, rec)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, errors.New(awserrors.ErrorEKSResourceInUse)
	}

	// Snapshot the CREATING reply before background launch (launch mutates rec).
	// Failures surface as CREATE_FAILED, which DeleteNodegroup reclaims.
	out := &eks.CreateNodegroupOutput{Nodegroup: nodegroupRecordToAWS(rec)}

	s.launchWG.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				s.markNodegroupFailed(acctKV, cluster, ng, fmt.Sprintf("launch panic: %v", r))
			}
		}()
		s.launchNodegroupInfra(nodegroupLaunchCtx{
			accountID: accountID,
			cluster:   cluster,
			ng:        ng,
			meta:      meta,
			rec:       rec,
			desired:   int(desired),
			acctKV:    acctKV,
		})
	})

	return out, nil
}

// nodegroupLaunchCtx carries the immutable inputs for an asynchronous nodegroup
// provisioning launch (launchNodegroupInfra). rec is mutated by the launch and
// must not be read by the caller after the goroutine starts.
type nodegroupLaunchCtx struct {
	accountID string
	cluster   string
	ng        string
	meta      *ClusterMeta
	rec       *NodegroupRecord
	desired   int
	acctKV    nats.KeyValue
}

// launchNodegroupInfra runs the slow provisioning tail of createNodegroup on a
// background goroutine after the record claim has been won: ensure SGs, decrypt
// the node token, resolve the eks-node AMI, launch the workers, and persist the
// terminal record state. Every failure marks the record CREATE_FAILED so the
// reclaim path (DeleteNodegroup) can tear it down.
func (s *EKSServiceImpl) launchNodegroupInfra(lc nodegroupLaunchCtx) {
	acctKV, accountID, cluster, ng, meta, rec := lc.acctKV, lc.accountID, lc.cluster, lc.ng, lc.meta, lc.rec

	cpSGID, ngSGID, err := EnsureClusterSGs(s.deps.VPCSG, accountID, cluster, meta.ResourcesVpcConfig.VpcId)
	if err != nil {
		s.markNodegroupFailed(acctKV, cluster, ng, "ensure cluster SGs: "+err.Error())
		return
	}
	if err := EnsureNodegroupSGRules(s.deps.VPCSG, accountID, cluster, cpSGID, ngSGID); err != nil {
		s.markNodegroupFailed(acctKV, cluster, ng, "ensure nodegroup SG rules: "+err.Error())
		return
	}

	token, err := s.decryptNodeToken(acctKV, cluster)
	if err != nil {
		s.markNodegroupFailed(acctKV, cluster, ng, "decrypt node token: "+err.Error())
		return
	}

	amiID, err := lookupEKSServerAMI(s.deps.Image, accountID)
	if err != nil {
		s.markNodegroupFailed(acctKV, cluster, ng, "resolve eks-node AMI: "+err.Error())
		return
	}

	if _, err := s.launchWorkers(acctKV, accountID, rec, meta, ngSGID, amiID, token, lc.desired); err != nil {
		// launchWorkers persisted each worker it launched (incrementally), so the
		// reclaim path can already tear them down; just record the terminal failure.
		rec.Status = eks.NodegroupStatusCreateFailed
		rec.StatusReason = "launch workers: " + err.Error()
		rec.ModifiedAt = time.Now().UTC()
		if perr := PutNodegroupRecord(acctKV, rec); perr != nil {
			slog.Error("createNodegroup: persist CREATE_FAILED record", "cluster", cluster, "nodegroup", ng, "err", perr)
		}
		return
	}

	// Gate ACTIVE on the workers registering Ready (observed via the CP state
	// report's Ready-node count), not merely on RunInstances success — a worker
	// that boots but never joins must surface CREATE_FAILED, not falsely ACTIVE.
	// Baseline is the create-time node count; the workers add lc.desired Ready nodes.
	if err := s.waitWorkersReady(acctKV, cluster, meta.NodeCount, lc.desired); err != nil {
		rec.Status = eks.NodegroupStatusCreateFailed
		rec.StatusReason = "workers did not become Ready: " + err.Error()
		rec.ModifiedAt = time.Now().UTC()
		if perr := PutNodegroupRecord(acctKV, rec); perr != nil {
			slog.Error("createNodegroup: persist CREATE_FAILED record", "cluster", cluster, "nodegroup", ng, "err", perr)
		}
		return
	}

	rec.Status = eks.NodegroupStatusActive
	rec.ModifiedAt = time.Now().UTC()
	if err := PutNodegroupRecord(acctKV, rec); err != nil {
		slog.Error("createNodegroup: persist ACTIVE record", "cluster", cluster, "nodegroup", ng, "err", err)
	}
}

// waitWorkersReady blocks until the cluster's Ready-node count rises by want over
// baseline — every nodegroup worker registered Ready — or the timeout / bgCtx
// fires. Ready count is meta.NodeCount, which the ClusterReconciler refreshes
// from the CP's NATS state report (Ready nodes only). Mirrors the CP reconciler's
// healthy-observe gating: a nodegroup is ACTIVE only once its workers are observed
// Ready, not merely launched.
func (s *EKSServiceImpl) waitWorkersReady(acctKV nats.KeyValue, cluster string, baseline, want int) error {
	target := baseline + want
	deadline := time.Now().Add(s.nodegroupReadyTimeout)
	for {
		meta, err := GetClusterMeta(acctKV, cluster)
		if err != nil {
			return err
		}
		if meta.NodeCount >= target {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s: cluster reports %d Ready nodes, want >= %d (baseline %d + %d workers)",
				s.nodegroupReadyTimeout, meta.NodeCount, target, baseline, want)
		}
		select {
		case <-s.bgCtx.Done():
			return s.bgCtx.Err()
		case <-time.After(s.nodegroupReadyPoll):
		}
	}
}

// launchWorkers issues count customer-owned RunInstances calls (one per worker
// so each gets a distinct node name + node label in its user-data) and returns
// the launched instance IDs. On a partial failure it returns the IDs that did
// launch plus the error so the caller can persist them for teardown.
func (s *EKSServiceImpl) launchWorkers(acctKV nats.KeyValue, accountID string, rec *NodegroupRecord, meta *ClusterMeta, ngSGID, amiID, token string, count int) ([]string, error) {
	instanceType := defaultNodegroupInstanceType
	if len(rec.InstanceTypes) > 0 {
		instanceType = rec.InstanceTypes[0]
	}
	subnet := rec.Subnets[0]

	// base is the worker set already on the record (non-empty on a scale-up).
	// Each incremental persist below writes base+newly-launched so the durable
	// record always reflects every live worker, never just this call's additions.
	base := append([]string(nil), rec.InstanceIDs...)
	ids := make([]string, 0, count)
	for i := range count {
		shortID := uuid.NewString()[:8]
		userData := buildAgentUserData(agentUserDataInput{
			ClusterName:   rec.ClusterName,
			NodegroupName: rec.Name,
			ServerURL:     meta.Endpoint,
			JoinToken:     token,
			NodeName:      fmt.Sprintf("%s-%s-%s", rec.ClusterName, rec.Name, shortID),
		})
		runInput := &ec2.RunInstancesInput{
			ImageId:          aws.String(amiID),
			InstanceType:     aws.String(instanceType),
			MinCount:         aws.Int64(1),
			MaxCount:         aws.Int64(1),
			SubnetId:         aws.String(subnet),
			SecurityGroupIds: aws.StringSlice([]string{ngSGID}),
			UserData:         aws.String(userData),
			TagSpecifications: []*ec2.TagSpecification{{
				ResourceType: aws.String("instance"),
				Tags: []*ec2.Tag{
					{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(rec.ClusterName)},
					{Key: aws.String(clusterEKSNodegroupTagKey), Value: aws.String(rec.Name)},
				},
			}},
		}
		if rec.DiskSize > 0 {
			runInput.BlockDeviceMappings = []*ec2.BlockDeviceMapping{{
				DeviceName: aws.String("/dev/vda"),
				Ebs:        &ec2.EbsBlockDevice{VolumeSize: aws.Int64(rec.DiskSize)},
			}}
		}
		res, err := s.deps.Worker.RunWorkerInstance(runInput, accountID)
		if err != nil {
			return ids, fmt.Errorf("run worker %d/%d: %w", i+1, count, err)
		}
		for _, inst := range res.Instances {
			if id := aws.StringValue(inst.InstanceId); id != "" {
				ids = append(ids, id)
			}
		}
		// Persist the launched worker IDs before issuing the next RunInstances so
		// a crash mid-loop leaves every live worker recorded and reclaimable (by
		// DeleteNodegroup or the boot reclaim sweep). Without this the record keeps
		// its claim-time InstanceIDs until the loop's terminal write, which strands
		// the already-launched workers if the daemon dies in between.
		rec.InstanceIDs = append(append([]string(nil), base...), ids...)
		if perr := PutNodegroupRecord(acctKV, rec); perr != nil {
			return ids, fmt.Errorf("persist launched workers %d/%d: %w", i+1, count, perr)
		}
	}
	return ids, nil
}

func (s *EKSServiceImpl) describeNodegroup(acctKV nats.KeyValue, input *eks.DescribeNodegroupInput) (*eks.DescribeNodegroupOutput, error) {
	cluster := aws.StringValue(input.ClusterName)
	ng := aws.StringValue(input.NodegroupName)
	if cluster == "" || ng == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	rec, err := GetNodegroupRecord(acctKV, cluster, ng)
	if err != nil {
		if errors.Is(err, ErrNodegroupNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	return &eks.DescribeNodegroupOutput{Nodegroup: nodegroupRecordToAWS(rec)}, nil
}

func (s *EKSServiceImpl) listNodegroups(acctKV nats.KeyValue, input *eks.ListNodegroupsInput) (*eks.ListNodegroupsOutput, error) {
	cluster := aws.StringValue(input.ClusterName)
	if cluster == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if _, err := GetClusterMeta(acctKV, cluster); err != nil {
		if errors.Is(err, ErrClusterNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	recs, err := ListNodegroupRecords(acctKV, cluster)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(recs))
	for _, rec := range recs {
		names = append(names, rec.Name)
	}
	return &eks.ListNodegroupsOutput{Nodegroups: aws.StringSlice(names)}, nil
}

func (s *EKSServiceImpl) updateNodegroupConfig(acctKV nats.KeyValue, input *eks.UpdateNodegroupConfigInput, accountID string) (*eks.UpdateNodegroupConfigOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	ng := aws.StringValue(input.NodegroupName)
	if cluster == "" || ng == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	meta, err := GetClusterMeta(acctKV, cluster)
	if err != nil {
		if errors.Is(err, ErrClusterNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}
	rec, err := GetNodegroupRecord(acctKV, cluster, ng)
	if err != nil {
		if errors.Is(err, ErrNodegroupNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}

	if input.ScalingConfig != nil {
		if v := input.ScalingConfig.MinSize; v != nil {
			rec.ScalingMin = *v
		}
		if v := input.ScalingConfig.MaxSize; v != nil {
			rec.ScalingMax = *v
		}
		if v := input.ScalingConfig.DesiredSize; v != nil {
			rec.ScalingDesired = *v
		}
	}
	if input.Labels != nil {
		rec.Labels = applyLabelUpdate(rec.Labels, input.Labels)
	}

	if err := s.reconcileWorkerCount(acctKV, accountID, rec, meta); err != nil {
		// Persist partial IDs so DeleteNodegroup can reclaim a failed scale-up.
		rec.ModifiedAt = time.Now().UTC()
		if perr := PutNodegroupRecord(acctKV, rec); perr != nil {
			slog.Error("updateNodegroupConfig: persist after reconcile failure",
				"cluster", cluster, "nodegroup", ng, "err", perr)
		}
		return nil, err
	}

	rec.ModifiedAt = time.Now().UTC()
	if err := PutNodegroupRecord(acctKV, rec); err != nil {
		return nil, err
	}

	return &eks.UpdateNodegroupConfigOutput{Update: &eks.Update{
		Id:        aws.String(uuid.NewString()),
		Status:    aws.String(eks.UpdateStatusSuccessful),
		Type:      aws.String(eks.UpdateTypeConfigUpdate),
		CreatedAt: aws.Time(rec.ModifiedAt),
	}}, nil
}

// reconcileWorkerCount launches or terminates workers so len(InstanceIDs)
// matches ScalingDesired. Surplus removal terminates the last (highest-index)
// instance IDs so scale-down is deterministic.
func (s *EKSServiceImpl) reconcileWorkerCount(acctKV nats.KeyValue, accountID string, rec *NodegroupRecord, meta *ClusterMeta) error {
	current := len(rec.InstanceIDs)
	desired := int(rec.ScalingDesired)

	switch {
	case desired > current:
		token, err := s.decryptNodeToken(acctKV, rec.ClusterName)
		if err != nil {
			return fmt.Errorf("decrypt node token: %w", err)
		}
		amiID, err := lookupEKSServerAMI(s.deps.Image, accountID)
		if err != nil {
			if errors.Is(err, ErrEKSServerAMINotFound) {
				return errors.New(awserrors.ErrorServiceUnavailable)
			}
			return fmt.Errorf("resolve eks-node AMI: %w", err)
		}
		_, ngSGID, err := EnsureClusterSGs(s.deps.VPCSG, accountID, rec.ClusterName, meta.ResourcesVpcConfig.VpcId)
		if err != nil {
			return fmt.Errorf("ensure cluster SGs: %w", err)
		}
		// launchWorkers appends to and persists rec.InstanceIDs incrementally.
		if _, err := s.launchWorkers(acctKV, accountID, rec, meta, ngSGID, amiID, token, desired-current); err != nil {
			return fmt.Errorf("launch workers: %w", err)
		}
	case desired < current:
		surplus := rec.InstanceIDs[desired:]
		if err := s.deps.Worker.TerminateWorkerInstances(surplus, accountID); err != nil {
			return fmt.Errorf("terminate surplus workers: %w", err)
		}
		rec.InstanceIDs = rec.InstanceIDs[:desired]
	}
	return nil
}

func (s *EKSServiceImpl) updateNodegroupVersion(input *eks.UpdateNodegroupVersionInput) (*eks.UpdateNodegroupVersionOutput, error) {
	// Worker AMI version upgrades (drain + replace) are not implemented in v1.
	if input != nil {
		slog.Info("UpdateNodegroupVersion not implemented in v1",
			"cluster", aws.StringValue(input.ClusterName), "nodegroup", aws.StringValue(input.NodegroupName))
	}
	return nil, notImpl()
}

func (s *EKSServiceImpl) deleteNodegroup(acctKV nats.KeyValue, input *eks.DeleteNodegroupInput, accountID string) (*eks.DeleteNodegroupOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	cluster := aws.StringValue(input.ClusterName)
	ng := aws.StringValue(input.NodegroupName)
	if cluster == "" || ng == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	rec, err := GetNodegroupRecord(acctKV, cluster, ng)
	if err != nil {
		if errors.Is(err, ErrNodegroupNotFound) {
			return nil, errors.New(awserrors.ErrorEKSResourceNotFound)
		}
		return nil, err
	}

	if len(rec.InstanceIDs) > 0 {
		if err := s.deps.Worker.TerminateWorkerInstances(rec.InstanceIDs, accountID); err != nil {
			return nil, fmt.Errorf("terminate workers: %w", err)
		}
	}
	if err := DeleteNodegroupRecord(acctKV, cluster, ng); err != nil {
		return nil, err
	}

	rec.Status = eks.NodegroupStatusDeleting
	return &eks.DeleteNodegroupOutput{Nodegroup: nodegroupRecordToAWS(rec)}, nil
}

// decryptNodeToken reads + decrypts the cluster's K3s join token from KV.
func (s *EKSServiceImpl) decryptNodeToken(kv nats.KeyValue, cluster string) (string, error) {
	if kv == nil {
		return "", errors.New("eks: nil KV for node token")
	}
	entry, err := kv.Get(NodeTokenKey(cluster))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return "", errors.New("eks: cluster join token not yet provisioned")
		}
		return "", fmt.Errorf("kv get node token: %w", err)
	}
	token, err := handlers_iam.DecryptSecret(string(entry.Value()), s.deps.MasterKey)
	if err != nil {
		return "", fmt.Errorf("decrypt node token: %w", err)
	}
	if token == "" {
		return "", errors.New("eks: decrypted node token is empty")
	}
	return token, nil
}

// reclaimOrphanedNodegroups terminates workers stranded by a daemon restart
// that interrupted createNodegroup. It runs once on boot
// (SpawnRegisteredReconcilers), before any launch goroutine from this process
// exists, so a record still in CREATING is definitionally orphaned: its launcher
// died with the prior process and nothing will ever drive it to a terminal
// state. Such records — and any CREATE_FAILED record still holding worker IDs —
// have their recorded workers terminated and the record settled to CREATE_FAILED
// with InstanceIDs cleared, which is idempotent on the next boot.
func (s *EKSServiceImpl) reclaimOrphanedNodegroups(accountID string, acctKV nats.KeyValue, cluster string) {
	recs, err := ListNodegroupRecords(acctKV, cluster)
	if err != nil {
		slog.Warn("reclaimOrphanedNodegroups: list records failed", "cluster", cluster, "err", err)
		return
	}
	for _, rec := range recs {
		stuckCreating := rec.Status == eks.NodegroupStatusCreating
		failedWithWorkers := rec.Status == eks.NodegroupStatusCreateFailed && len(rec.InstanceIDs) > 0
		if !stuckCreating && !failedWithWorkers {
			continue
		}
		if len(rec.InstanceIDs) > 0 {
			if err := s.deps.Worker.TerminateWorkerInstances(rec.InstanceIDs, accountID); err != nil {
				// Leave the record untouched so the next boot retries the reclaim
				// rather than orphaning the workers by clearing their IDs.
				slog.Error("reclaimOrphanedNodegroups: terminate workers failed",
					"cluster", cluster, "nodegroup", rec.Name, "instances", rec.InstanceIDs, "err", err)
				continue
			}
		}
		reason := rec.StatusReason
		if stuckCreating {
			reason = "create interrupted by daemon restart; stranded workers reclaimed"
		}
		priorStatus := rec.Status
		workerCount := len(rec.InstanceIDs)
		rec.Status = eks.NodegroupStatusCreateFailed
		rec.StatusReason = reason
		rec.InstanceIDs = nil
		rec.ModifiedAt = time.Now().UTC()
		if err := PutNodegroupRecord(acctKV, rec); err != nil {
			slog.Warn("reclaimOrphanedNodegroups: persist settled record failed",
				"cluster", cluster, "nodegroup", rec.Name, "err", err)
			continue
		}
		slog.Info("reclaimOrphanedNodegroups: reclaimed stranded nodegroup workers",
			"cluster", cluster, "nodegroup", rec.Name, "priorStatus", priorStatus, "workers", workerCount)
	}
}

func (s *EKSServiceImpl) markNodegroupFailed(kv nats.KeyValue, cluster, ng, reason string) {
	rec, err := GetNodegroupRecord(kv, cluster, ng)
	if err != nil {
		return
	}
	rec.Status = eks.NodegroupStatusCreateFailed
	rec.StatusReason = reason
	rec.ModifiedAt = time.Now().UTC()
	if err := PutNodegroupRecord(kv, rec); err != nil {
		slog.Warn("markNodegroupFailed: persist failed", "cluster", cluster, "nodegroup", ng, "err", err)
	}
}

// scalingFromInput derives min/max/desired from a NodegroupScalingConfig,
// defaulting to a single node when the caller omits the config or fields.
func scalingFromInput(cfg *eks.NodegroupScalingConfig) (minSize, maxSize, desired int64) {
	minSize, maxSize, desired = 1, 1, 1
	if cfg == nil {
		return minSize, maxSize, desired
	}
	if cfg.MinSize != nil {
		minSize = *cfg.MinSize
	}
	if cfg.DesiredSize != nil {
		desired = *cfg.DesiredSize
	} else {
		desired = minSize
	}
	if cfg.MaxSize != nil {
		maxSize = *cfg.MaxSize
	} else if desired > maxSize {
		maxSize = desired
	}
	return minSize, maxSize, desired
}

// applyLabelUpdate applies an UpdateLabelsPayload (addOrUpdate + removeLabels)
// onto the record's label map.
func applyLabelUpdate(existing map[string]string, payload *eks.UpdateLabelsPayload) map[string]string {
	if payload == nil {
		return existing
	}
	out := map[string]string{}
	maps.Copy(out, existing)
	for k, v := range payload.AddOrUpdateLabels {
		out[k] = aws.StringValue(v)
	}
	for _, k := range payload.RemoveLabels {
		delete(out, aws.StringValue(k))
	}
	return out
}

func nodegroupRecordToAWS(rec *NodegroupRecord) *eks.Nodegroup {
	if rec == nil {
		return nil
	}
	out := &eks.Nodegroup{
		ClusterName:   aws.String(rec.ClusterName),
		NodegroupName: aws.String(rec.Name),
		NodegroupArn:  aws.String(rec.Arn),
		Status:        aws.String(rec.Status),
		Subnets:       aws.StringSlice(rec.Subnets),
		InstanceTypes: aws.StringSlice(rec.InstanceTypes),
		AmiType:       aws.String(rec.AMIType),
		NodeRole:      aws.String(rec.NodeRole),
		Version:       aws.String(rec.Version),
		CreatedAt:     aws.Time(rec.CreatedAt),
		ModifiedAt:    aws.Time(rec.ModifiedAt),
		ScalingConfig: &eks.NodegroupScalingConfig{
			MinSize:     aws.Int64(rec.ScalingMin),
			MaxSize:     aws.Int64(rec.ScalingMax),
			DesiredSize: aws.Int64(rec.ScalingDesired),
		},
		Resources: &eks.NodegroupResources{},
	}
	if rec.DiskSize > 0 {
		out.DiskSize = aws.Int64(rec.DiskSize)
	}
	if len(rec.Labels) > 0 {
		out.Labels = aws.StringMap(rec.Labels)
	}
	if rec.StatusReason != "" {
		out.Health = &eks.NodegroupHealth{Issues: []*eks.Issue{{
			Code:        aws.String(eks.NodegroupIssueCodeNodeCreationFailure),
			Message:     aws.String(rec.StatusReason),
			ResourceIds: aws.StringSlice(rec.InstanceIDs),
		}}}
	}
	return out
}
