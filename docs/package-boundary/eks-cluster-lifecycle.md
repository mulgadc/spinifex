# EKS cluster: delete lifecycle contract

Contract under ADR-0003 S1 for the delete half of an EKS cluster's lifecycle, the half ADR-0003 S8 constrains.
Create, update and the control-plane reconciler are out of its scope and keep the observed gaps recorded elsewhere.
Paths are relative to `spinifex/` unless they start with `tests/`, which is relative to the spinifex repo root.
Test names without a path are in `domains/eks/cluster/`.

## Scope

A cluster is a composite resource: its record at `clusters/{name}/meta` in the `eks-account-{account}` bucket names the dependants a delete must remove.
In the system account those are the control-plane VMs and their root volumes, the endpoint load balancer with its target groups, the managed control-plane VPC and its network, and the spread placement group.
In the customer account they are the egress address, the private-endpoint interface and security group, the cluster security groups, a cluster-owned internet gateway, and load balancers the in-cluster controller created.
Node group workers and CSI-provisioned EBS volumes are never removed by a cluster delete.

## Contract

| Item | Behaviour | Evidence |
|---|---|---|
| Request | `DeleteCluster` validates, answers `ResourceNotFoundException` for an absent cluster, answers a `DELETING` cluster from its record, refuses a cluster with node groups with `ResourceInUseException`, records `DELETING` and returns. It waits for no teardown step. | `TestS8_EKSDeleteClusterReturnsBeforeTheTeardownCompletes`, `TestRLC1_EKSDeleteClusterNotFoundOnAbsent`, `TestS8_EKSDeleteClusterRefusesWhileNodegroupsExist`, `tests/e2e/eks` `TestEKS/DeleteCluster` |
| Obligation | `DELETING` with `DeletingSince` on the record is the tenant's decision. No status writer moves a record off it, and a create launch still running adds its refs to it instead of overwriting it. | `TestS2_EKSDeletingIsStickyForStatusWriters`, `TestS2_EKSLaunchWriteAfterDeleteNeitherRevertsNorRecreates` |
| Retry | A repeat delete answers `DELETING` from the record and starts nothing. | `TestS8_EKSSecondDeleteAnswersFromTheRecordWithoutASecondTeardown` |
| Convergence | `convergeClusterDeletion` (`domains/eks/cluster/cluster_teardown.go`), started by the request and backed by `EKSDeletingReaper`, runs under a per-cluster in-process gate and the cross-node teardown lease, renewed on the service context. It acts only on a record that is `DELETING` when re-read. | `TestS8_EKSKickAndSameNodeSweepRunOneTeardown`, `TestS8_EKSTeardownLeaseOutlivesTheRequest`, `TestS7_EKSTeardownActsOnlyOnARecordedDelete` |
| Retained cleanup | A failed step keeps the record `DELETING` with its refs and the error. The record is removed last, after its child records, and only if no ref appeared that the pass did not tear down. A record with a node group is never removed. | `TestRLC2_EKSBillableTeardownBeforeKVSweep`, `TestS2_EKSTeardownRemovesTheMetaLast`, `TestS2_EKSTeardownDoesNotSweepARecordChangedDuringTeardown`, `TestS7_EKSTeardownNeverRemovesAClusterWithNodegroupWorkers` |
| Recovery | A teardown interrupted by a crash or a lost lease is finished by the reaper on any node. Failed passes back off from 90 s, doubling to a 30 min cap, and are never abandoned; `DeleteReapExhausted` only flags a long failure for an operator. | `TestS2_EKSTeardownResumesAfterAnInterruptedConverge`, `TestRLC4_DeletingReaperReDrivesWedgedTeardown`, `TestDeletingReaper_KeepsRetryingPastTheOldAttemptLimit`, `TestDeletingReaper_ResumesAnAlreadyExhaustedRecord` |
| Visibility | `DescribeCluster` and `ListClusters` show the cluster `DELETING` until the record is removed, then `ResourceNotFoundException`. A failed pass shows one `INTERNAL_FAILURE` health issue naming the step, never the internal error. | `TestDescribeCluster_StuckDeleteShowsTheStepNotTheError`, `TestRLC2_EKSBillableTeardownBeforeKVSweep` |
| New dependants | `CreateNodegroup` requires `ACTIVE`; `CreateAddon` and `CreateAccessEntry` refuse a `DELETING` cluster with `ResourceInUseException`. | `TestCreateAddonRefusesOnlyADeletingCluster`, `TestS2_EKSDeletingClusterAdmitsNoNewDependants` |
| Subject budgets | Every `eks.*` request subject is classified by its handler's work in `eksSubjects` (`domains/eks/cluster/service_nats.go`) and keeps the 30 s budget; `eks.DeleteCluster` is request-scaled. A subject the gateway sends or the daemon serves that is not classified fails the table. | `TestS8_EKSSubjectBudgets` |
| Late child records | No child record outlives the cluster or leaks into a later one of the same name. The teardown sweeps child keys again after removing the meta and stops the cluster's bootstrap subscriber, bootstrap refuses to write into a record that is gone or `DELETING`, the launch re-checks its record around seeding the creator's access entry, and a fresh claim clears whatever an earlier cluster left under the name. | `TestS2_EKSLaunchOvertakenByDeleteLeavesNoChildRecords`, `TestS2_EKSBootstrapWritesNothingIntoADeletingCluster`, `TestS2_EKSFreshClaimInheritsNoStaleChildRecords` |
| Launch generation | The create launch's writes are fenced on the record's generation (`CreatedAt` and ARN). An overtaken launch never writes a newer cluster of the same name and releases only the instances it started, by ID. One whose record was swept recreates it as `DELETING` with its refs, so the teardown and reaper own the release. One whose record was marked `FAILED` merges its refs and releases what it built. | `TestS2_EKSOvertakenLaunchNeverWritesANewerClusterOfTheSameName`, `TestS2_EKSOvertakenLaunchHandsItsInfraToTheTeardown`, `TestS2_EKSLaunchMarkedFailedReleasesWhatItBuilt` |
| Bounded passes | One converger run is bounded at 15 minutes, so a hung step cannot hold the gate and lease; the timed-out pass is recorded and backed off. Waiting on node groups is not counted as a failed pass, and deleting the last node group resumes the delete. | `TestS8_EKSHungTeardownStepReleasesTheGate`, `TestS2_EKSLastNodegroupDeletedResumesTheClusterDelete` |
| FAILED reclaim | Re-creating a `FAILED` cluster's name tears the old attempt down under the same gate and lease as a delete, and answers `ResourceInUseException` while another holder has it. | `TestS2_EKSFailedClusterReclaimRunsUnderTheTeardownLease` |
| Tenant data | The teardown never deletes a CSI-provisioned volume or terminates a node group worker. | `TestS7_EKSTeardownNeverDeletesCSIVolumesOrWorkers`, `TestS7_EKSTeardownNeverRemovesAClusterWithNodegroupWorkers` |

## Gaps

1. Four `eks.*` handlers still hold their request for work that grows with the resource, and the subject table names them as not conformant: `CreateCluster` (reclaiming a `FAILED` cluster of the same name tears its infra down inline), `DeleteNodegroup` (one awaited terminate per worker), `UpdateNodegroupConfig` (one awaited launch or terminate per worker added or removed) and `RestoreSnapshot` (launches a control-plane VM and fences the old one inline).
2. AWS's answer to a repeat delete on a `DELETING` cluster has not been observed; `DELETING` from the record is the chosen behaviour.
3. Control-plane, load balancer and VPC resources in the system account are keyed by the bare cluster name, so a teardown by name can reach another tenant's same-named cluster.
4. The teardown deletes load balancers the in-cluster controller created in the customer account, which AWS leaves behind.
5. An overtaken launch's late write can still land under a newer cluster of the same name in the instant between that cluster's claim sweep and the old launch's generation check; it is bounded by one write, since the old launch stops at its next fenced write.
6. The `FAILED`-name reclaim purge still runs inside the `CreateCluster` request (gap 1).
