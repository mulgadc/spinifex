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

## Gaps

1. No table asserts a budget for every `eks.*` NATS subject, which ADR-0003 S8 asks for; every subject still inherits `defaultTimeout` in `domains/eks/cluster/service_nats.go`.
2. AWS's answer to a repeat delete on a `DELETING` cluster has not been observed; `DELETING` from the record is the chosen behaviour.
3. Control-plane, load balancer and VPC resources in the system account are keyed by the bare cluster name, so a teardown by name can reach another tenant's same-named cluster.
4. The teardown deletes load balancers the in-cluster controller created in the customer account, which AWS leaves behind.
