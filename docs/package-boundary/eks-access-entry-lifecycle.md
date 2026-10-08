# EKS access entry: lifecycle contract

Present contract under ADR-0003 S1, recorded before the resource moves.
It describes the code as it stands, so it claims no conformance; the gaps below are observed behaviour, not targets.
Paths are relative to `spinifex/` unless they start with `tests/`.

## Scope

An access entry has no realization of its own.
Create, update, delete and policy association complete synchronously within the request as writes to the account's EKS bucket; the token webhook reads the record when it authenticates a `get-token` bearer.

## Contract

| Item | Present behaviour | Evidence |
|---|---|---|
| Identity | Cluster name plus principal ARN. Key and ARN suffix use `sha256hex(principalARN)` (`handlers/eks/store.go` `PrincipalARNHash`). ARN `arn:aws:eks:{region}:{account}:access-entry/{cluster}/{hash}` (`foundation/aws/arn/eks.go`); the AWS ARN shape differs and is not verified. | `handlers/eks/service_impl_test.go` `TestAccessEntry_CreateDescribeListDelete`, `handlers/eks/store_test.go`, `gateway/eks/authz_test.go` `TestResourceARNsAccessEntryMatchesHandler` |
| Scope | Account and Region, within one cluster: keys live under `clusters/{cluster}/access-entries/` in the `eks-account-{account}` bucket. | `handlers/eks/access_lifecycle_test.go` `TestAccessEntryPersistedLayout_TopLevelAndNestedFieldNames`, `TestAccessEntryRecord_DecodesHandWrittenJSON`, `handlers/eks/create_cluster_test.go` `TestCreateCluster_SeedsCreatorAdminAccessEntry` |
| Durable owner and record | `handlers/eks/access_entry.go` writes `AccessEntryRecord` with nested `AssociatedAccessPolicy` and `AccessScope` (`handlers/eks/types.go`). The cluster launch also writes the creator-admin entry, and `DeleteClusterPrefix` (`handlers/eks/cluster_state.go`) erases entries with the cluster. The gateway process reads records directly through `ResolveTokenReview` (`handlers/eks/token_review.go`). | `handlers/eks/access_lifecycle_test.go` `TestAccessEntryPersistedLayout_TopLevelAndNestedFieldNames`, `TestAccessEntryRecord_DecodesHandWrittenJSON`, `handlers/eks/create_cluster_test.go` `TestCreateCluster_SeedsCreatorAdminAccessEntry` |
| Desired state | Principal, Kubernetes username and groups, type (`STANDARD` only), tags (set at create only) and cluster-scoped access policy associations. | `handlers/eks/service_impl_test.go` `TestAccessEntry_RejectsNodeType`, `TestAccessPolicy_AssociateRejectsUnsupportedPolicyAndScope`, `TestAccessPolicy_AssociateRejectsNamespaceScope`, `handlers/eks/access_lifecycle_test.go` `TestAccessEntry_UpdateWithNoFieldsKeepsUsernameAndGroups` |
| Generation | None. Update, associate and disassociate retry against the KV revision; create and delete do not. | |
| Observed state | Not tracked; the record is the only state. | |
| Readiness | Usable by token review as soon as the write returns. | `handlers/eks/token_review_test.go` `TestResolveTokenReview_AuthenticatesViaVerifyAndKV`, `TestResolveTokenReview_NoAccessEntryDenies`, `handlers/eks/service_impl_test.go` `TestAccessPolicy_DisassociateRemovesProjectedGroup` |
| Idempotency | `clientRequestToken` is ignored. Update always advances `modifiedAt`, even when nothing changes. A duplicate create returns `ResourceInUseException`; a repeated delete returns `ResourceNotFoundException`; disassociating an unbound policy succeeds without change. Internal NATS redelivery of create returns `ResourceInUseException` to the redelivered request. | `handlers/eks/service_impl_test.go` `TestAccessEntry_CreateDescribeListDelete`, `TestAccessEntry_DescribeDeleteMissingIsNotFound`, `handlers/eks/access_lifecycle_test.go` `TestAccessPolicy_DisassociateUnassociatedLeavesRecordUnchanged`, `TestAccessPolicy_AssociateWithNoEntryIsNotFound` |
| Dependencies | Every action requires only that the cluster's meta record exists (`acctKVForCluster`); cluster status is not read, so actions succeed on a `CREATING`, `FAILED` or `DELETING` cluster. Token review does not check the cluster at all. The principal is not checked against IAM. | `handlers/eks/service_impl_test.go` `TestAccessEntry_UnknownClusterIsNotFound`, `handlers/eks/access_lifecycle_test.go` `TestAccessEntryActionsIgnoreClusterLifecycleStatus`, `TestResolveTokenReview_AuthenticatesWithoutClusterMeta` |
| Authorization | Gateway authz derives the access-entry ARN with `PrincipalARNHash` (`gateway/eks/authz.go`). No `iam:PassRole`. The STS step of token review is the existing `eks.VerifyToken` NATS contract served by awsgw. | `gateway/eks/authz_test.go` `TestResourceARNsAccessEntryMatchesHandler` |
| Deletion ordering | Delete is get-then-delete, not conditional. Cluster delete erases entries in the prefix purge only after all infrastructure teardown succeeds; on failure they survive while the cluster stays `DELETING`. | `handlers/eks/access_lifecycle_test.go` `TestDeleteClusterPrefix_SweepsAccessEntriesScopedToCluster` |
| Restart and interruption | The prefix purge deletes keys in listing order; an interruption after the meta key is gone leaves entries that a new cluster of the same name inherits. Reclaiming a `FAILED` cluster and a failed launch purge with the meta kept, so the recreated cluster inherits the failed attempt's entries. | none |
| End-to-end | `tests/e2e/eks/eks_test.go` `AccessEntry` and `GetToken`; `tests/integration/cross_tenant_test.go`. No Terraform apply/destroy evidence for `aws_eks_access_entry` or `aws_eks_access_policy_association`. | |

## Gaps

1. Create is get-then-put with no exclusive create, so concurrent creates for one principal both succeed and the later write wins.
2. The creator-admin seed at cluster launch is an unconditional put after the cluster meta is final, so it replaces an entry for the same principal created while the cluster was `CREATING`, including its policy associations.
3. Access actions do not check cluster status (see dependencies row), and opening the bucket creates it for an account that has none.
4. Entries can outlive their cluster (interrupted prefix purge) or carry over to a recreated cluster (reclaim and failed launch keep the meta and do not purge entries separately).
5. Token review authenticates against an entry without checking the cluster exists or its status.
6. `ResolveTokenReview` opens the EKS bucket and reads the record in the gateway process, outside the owner.
7. List ignores `maxResults` and `nextToken`; tagging an access-entry ARN returns `NotImplemented`.
8. No ADR-0003 S5 failure or recovery evidence.
