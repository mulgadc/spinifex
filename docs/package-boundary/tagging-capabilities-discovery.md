# Tagging capabilities discovery

**Status:** Discovery only.
Nothing here is a decision.
It records current behaviour at the head of the ADR-0001 checkpoint branch and proposes a capability shape for review.

## Scope and method

This covers every Spinifex path that reads or writes resource tags, grouped by the owner of the record the tags live on.
File references are relative to the repository root at that revision.

What was measured:

- Code reading of every tag entry point found by searching for `Tag`/`Untag`/`AddTags`/`RemoveTags`/`ListTags`/`DescribeTags` handlers and the EC2 record-mirror fan-out.
- Sixteen characterisation tests named `TestCurrentBehaviour_Tagging_*` (listed under [Tests](#tests)), run through real handlers on embedded JetStream.
- AWS documentation fetched on 2026-10-13 for the pages cited in [AWS parity](#aws-parity).

What was not measured:

- No live cluster run, so multi-node interleavings are shown by injecting a write between a handler's read and its write, not by real concurrent nodes.
- No test drives the full daemon CreateTags path including record mirrors on a multi-node cluster.
- Predastore's own S3 bucket tagging was not exercised; Spinifex does not read it.

Related work this does not duplicate:

- `mulga-6maav`: CreateTags accepts unvalidated or nonexistent resource IDs.
- `mulga-5eh43`: IAM CreatePolicy rejects `aws:RequestTag` condition keys.
- `mulga-lmq4l`: tag-based ABAC condition keys.
- `docs/development/bugs/concurrent-createtags-lost-updates.md` (mulga repo): moved the EC2 central index to JetStream KV under CAS (`mulga-tt77o`, closed).
  That plan's "Not done" list leaves the two-store split in place, which is where the record-mirror finding below sits.
- `docs/development/bugs/creation-time-tags-describetags.md` (mulga repo): creation-time tags reach the central index only for some EC2 owners.

## 1. Map of tag reads and writes by owner

### EC2 and the VPC family

EC2 has two tag stores.

- **Central index:** JetStream KV bucket `ec2-tags`, key `{accountID}.{resourceID}`, value the whole tag map, owned by `TagsServiceImpl` (`spinifex/spinifex/domains/ec2/tags/service_impl.go:50`).
  A legacy Predastore object `tags/{accountID}/{resourceID}.json` is read through and deleted on teardown (`service_impl.go:129`, `:156`).
  `DescribeTags` and `GetResources` read only this store.
- **Resource record:** each owning service's own KV or metadata record carries a `Tags` map.
  Each resource's own describe call and its tag filters read only this.

Write paths:

| Path | Package | Writes | Concurrency |
| --- | --- | --- | --- |
| AWSGW `CreateTags` validation | `domains/ec2/awsapi/tags/CreateTags.go:15` | nothing; refuses empty key and `aws:` prefix | n/a |
| Daemon `createTags` / `deleteTags` | `daemon/daemon_handlers_tag.go:89`, `:125` | central index for non-instance IDs, then every record mirror, then instance owners | sequential, first error returns |
| Central index merge | `domains/ec2/tags/service_impl.go:195`, `:377`; `store.go:77` | `ec2-tags` | CAS via `kvutil.Update` |
| Central index overwrite | `domains/ec2/tags/store.go:102` (`PutResourceTags`) | `ec2-tags` | CAS put |
| Record mirrors (`ApplyRecordTags`/`RemoveRecordTags`) | vpc `domains/ec2/vpc/service_impl.go:1091`, `:1110`, `:1166`; image `domains/ec2/image/service_impl.go:681`; volume `domains/ec2/volume/service_impl.go:1175`, `:1208`; snapshot `domains/ec2/snapshot/service_impl.go:997`; route table `domains/ec2/routetable/service_impl.go:1588`; igw `domains/ec2/igw/service_impl.go:598`; eigw `domains/ec2/eigw/service_impl.go:277`; eip `domains/ec2/eip/service_impl.go:938`; natgw `domains/ec2/natgw/service_impl.go:538`; key pair `domains/ec2/key/service_impl.go:1032` | each owner's record | plain read then `Put` (`domains/ec2/tagmirror/tagmirror.go:83`), no revision |
| Instance tags | `daemon/daemon_handlers_tag.go:194` routes to the owning daemon over `ec2.cmd.<id>`, or `TagStoppedInstance` when no responder | instance record and central index together | ownership-gated |
| Creation-time `TagSpecifications` | each create path | record; central index only for instance, vpc/subnet/sg/eni (`domains/ec2/vpc/service_impl.go`, `projectRecordTags`) and key pair (`domains/ec2/key/service_impl.go:55`) | central write failure is logged and swallowed |

The daemon fan-out (`daemon/daemon_handlers_tag.go:34`) calls every mirror with every non-instance ID, and each mirror skips IDs whose prefix it does not own.
`launchtemplate` and `placementgroup` are not in the mirror list, so a `CreateTags` on them reaches only the central index.
That last point is from code and is not tested.

Read paths:

- `DescribeTags` lists every key in the bucket and filters by account prefix (`domains/ec2/tags/service_impl.go:243`, `:290`).
  An ID with no recognised prefix is reported with resource type `unknown` (`:84`).
- Resource describes filter on their own record's tags (for example `domains/ec2/instance/service_impl.go:2040`).

Cross-owner reach: the daemon is the composition point that writes the central index and then reaches every record owner; the record mirrors each write only their own record.

### ELBv2

- **Store:** load balancer, target group, listener and rule records, each with a `Tags` map (`spinifex/spinifex/handlers/elbv2/service_impl_tags.go:25`).
- **Writes:** `AddTags` and `RemoveTags` (`service_impl_tags.go:101`, `:149`).
  Load balancer, listener and rule tag writes re-put the whole record read earlier (`:41`, `:74`, `:88`).
  Target group writes go through `UpdateTargetGroupByArn` CAS, but the merged map is computed from the earlier read and assigned inside the CAS closure (`:57`).
- **Reads:** `DescribeTags` returns one entry per ARN, tagged or not (`handlers/elbv2/service_impl.go:3041`).
- **Cross-owner reach:** `gateway/tagging` calls `DescribeLoadBalancers`, `DescribeTargetGroups` and `DescribeTags` over NATS (`spinifex/spinifex/gateway/tagging/GetResources.go:106`).

### RDS

- **Store:** DB instance, DB snapshot, subnet group and parameter group records in the account bucket (`spinifex/spinifex/handlers/rds/tags.go:56`).
- **Writes:** `AddTagsToResource`, `RemoveTagsFromResource` (`tags.go:104`, `:141`) through a registry of `TaggedRecord` kinds, with a revision-checked update retried up to 16 times (`tags.go:207`).
- **Validation:** 50 tags after merge, 128-character key, 256-character value, `aws:` refused (`tags.go:21`, `:236`).
- **ARN check:** region and account must match the caller (`handlers/rds/arn.go:66`, `:69`).
- **Cross-owner reach:** `handlers/rds/tags.go` writes `domains/rds/subnetgroup` and `domains/rds/parametergroup` records directly (`tags.go:53`, `:67`, `:72`).
  These are the legacy edges recorded in `docs/PACKAGE_BOUNDARY_MIGRATION.md:452` and `:453`.

### ECS

- **Store:** cluster, task definition revision, service, task and container instance records in the account bucket (`spinifex/spinifex/handlers/ecs/tags.go:77`).
- **Writes:** `TagResource`, `UntagResource` (`tags.go:228`, `:255`) via `mutateResourceTags` (`tags.go:141`), which re-puts the whole record with no revision.
- **ARN check:** `parseResourceARN` keeps only the resource segment (`tags.go:41`); the ARN's account and region are not compared with the caller's.
- **Validation:** none on count, length or prefix.
- **Cross-owner reach:** reads and writes `domains/ecs/taskdefinition` records directly (`tags.go:89`, `:154`), the legacy edge at `docs/PACKAGE_BOUNDARY_MIGRATION.md:454`.

### EKS

- **Store:** cluster meta and nodegroup records (`spinifex/spinifex/handlers/eks/service_impl.go:1763`).
- **Writes:** `TagResource`, `UntagResource` (`:1763`, `:1806`) under CAS (`casUpdateMeta`, `casUpdateNodegroupTags`).
  Other EKS ARNs return NotImplemented.
- **ARN check:** `clusterNameFromARN` and `nodegroupRefFromARN` discard account and region (`:1925`, `:1940`).
- **Validation:** none on count, length or prefix.

### ECR

- **Store:** repository meta record (`RepoMeta.Tags`) in the account's ECR KV bucket (`spinifex/spinifex/domains/ecr/meta.go:163`).
- **Writes:** `TagResource`, `UntagResource` in the AWS adapter (`spinifex/spinifex/domains/ecr/awsapi/tags.go:59`, `:90`), which reads the whole meta over NATS, edits it in the gateway process, and puts it back with a plain `kv.Put`.
- **ARN check:** `RepositoryNameFromResourceARN` keeps only the repository name (`domains/ecr/awsapi/authz.go:217`).
- **Validation:** missing or empty key only (`domains/ecr/awsapi/validation.go:64`).

### ACM

- **Store:** certificate record (`CertRecord.Tags`) (`spinifex/spinifex/domains/acm/store.go:223`).
- **Writes:** `AddTagsToCertificate`, `RemoveTagsFromCertificate` (`spinifex/spinifex/domains/acm/service_impl.go:724`, `:751`), through `Store.UpdateTags` (`store.go:502`).
  Both check ownership through `lookupOwned`, then change only `Tags` through `Store.UpdateTags`, a revision-checked update of the raw record that leaves the encrypted private key untouched.
  Before `mulga-ncn9f` was fixed they wrote back the `GetCertMetadata` copy through `PutCert`, which replaced the stored key with an empty one.
- **Validation:** none on count, length or prefix.

### IAM

- **Store:** role, user, customer-managed policy, instance profile and OIDC provider records in IAM KV buckets.
- **Writes:** `TagRole`/`UntagRole` (`spinifex/spinifex/handlers/iam/roles.go:521`, `:545`), `TagPolicy`/`UntagPolicy` (`handlers/iam/service_impl.go:1942`, `:1965`) and `TagInstanceProfile`/`UntagInstanceProfile` (`handlers/iam/instance_profiles.go:300`, `:325`) use CAS.
  `TagUser`/`UntagUser` (`handlers/iam/service_impl.go:1871`, `:1893`, `:1919`) and `TagOpenIDConnectProvider`/`UntagOpenIDConnectProvider` (`handlers/iam/oidc_providers.go:293`, `:311`, `:334`) read and then plain `Put`.
- **Validation:** count, key and value length, pattern, `aws:` prefix and duplicate keys (`handlers/iam/service_impl.go:279`).

### Resources with no Spinifex tag path

- Lambda and CloudWatch Logs have no package under `spinifex/spinifex/`.
- Bedrock (`gateway/bedrock*`, `domains/ochre`) has no tag action.
- S3 bucket tagging is served by Predastore (`predastore/internal/gate/handlers/bucket_tagging.go`); Spinifex's `GetResources` does not list buckets.

### Cross-service tagging API

- `gateway/tagging.go:19` serves only `GetResources`; any other action, including `TagResources`, `UntagResources`, `GetTagKeys` and `GetTagValues`, returns `InvalidAction` (`gateway/tagging.go:41`).
- Authorization evaluates `tagging:GetResources` against `*` (`spinifex/spinifex/gateway/tagging/authz.go:28`).
- `GetResources` aggregates ELBv2 load balancers and target groups plus the EC2 central index (`gateway/tagging/GetResources.go:45`), then filters by tag, sorts by ARN and slices by an integer offset token (`:264`).
- The layering ratchet records the two cross-owner imports as debt (`architecture/layering/allowlist_test.go:125`, `:126`), and `gateway/tagging` is a frozen legacy package (`architecture/layering/legacy_test.go:24`).

### Authorization against tags

- The policy evaluator allowlists five condition keys and none read a tag (`bluebottle/pkg/iampolicy/conditions.go`).
- IAM `CreatePolicy` refuses any unsupported key at write time (`handlers/iam/service_impl.go:2496`), with a specific reason for `aws:RequestTag/` (`:2514`).
- No code path reads a resource's tags to evaluate a request.

### Summary table

| Owner | Tag store | Service write API | Write concurrency | Limits enforced | ARN account checked | Listed by `GetResources` | Cross-owner reach |
| --- | --- | --- | --- | --- | --- | --- | --- |
| EC2 central index | `ec2-tags` KV | `CreateTags`/`DeleteTags` | CAS | `aws:` prefix only, at AWSGW | IDs are not ARNs; stored under caller account | yes | daemon fans out to record owners |
| EC2 record owners (11 services) | own record | via daemon mirror | plain read then put | none | n/a | no (index only) | none |
| EC2 instance | instance record and index | via owning daemon | ownership-gated | `aws:` at AWSGW | n/a | yes, via index | none |
| ELBv2 | LB/TG/listener/rule records | `AddTags`/`RemoveTags` | LB, listener, rule plain put; TG CAS over a stale merge | empty key only | owner field compared | LB and TG only | `gateway/tagging` reads over NATS |
| RDS | four record kinds | `AddTagsToResource`/`RemoveTagsFromResource` | CAS, 16 attempts | 50/128/256, `aws:` | yes, and region | no | writes `domains/rds` subnet and parameter group records |
| ECS | five record kinds | `TagResource`/`UntagResource` | plain read then put | none | no | no | writes `domains/ecs/taskdefinition` records |
| EKS | cluster meta, nodegroup | `TagResource`/`UntagResource` | CAS | none | no | no | none |
| ECR | repo meta | `TagResource`/`UntagResource` | plain read then put, from the gateway process | empty key only | no | no | none |
| ACM | certificate record | `AddTagsToCertificate`/`RemoveTagsFromCertificate` | revision-checked update of the tag field only | none | owner checked via `lookupOwned` | no | none |
| IAM | five record kinds | `Tag*`/`Untag*` | CAS for role, policy, instance profile; plain put for user, OIDC provider | full | names, not ARNs, except policy and OIDC | no | none |

## 2. Error and atomicity behaviour

Each line states current behaviour and how it was established.
"Test" names a `TestCurrentBehaviour_Tagging_*` test listed under [Tests](#tests); "code" means read from source and not tested here; "INFERRED" means a consequence of code that no test demonstrates.

### Read-modify-write and revision checks

- EC2 record mirror: a tag write and a non-tag record change that both land between the mirror's read and its put are reverted, and every call reports success.
  Test: `RecordMirrorRevertsInterleavedWrites`.
  Every record owner that routes through `tagmirror.UpdateKVRecordTags` shares this; volume and image use their own read-then-put with the same shape (INFERRED for those two).
- EC2 central index: CAS, so concurrent merges on one resource keep each other's keys.
  Code, and the closed plan's tests.
- ECS: the tag write reverts a record change committed between its read and its put.
  Test: `ECSTagWriteRevertsInterleavedRecordWrite` (an interleaved `DesiredCount` change is lost).
- ACM: tag writes are revision-checked and change only the tag field; the stored private key survives (resolved by `mulga-ncn9f`).
  Test: `ACMTagWritesPreserveStoredPrivateKey`.
  A certificate that lost its key before the fix can no longer be served by any listener that loads it (INFERRED); recovery is `mulga-o436p`.
- ELBv2: load balancer, listener and rule tag writes can revert a concurrent record change, and the target group CAS assigns a map merged from an earlier read, so two concurrent `AddTags` on one target group can lose a key.
  INFERRED from `service_impl_tags.go:41`, `:57`, `:74`, `:88`.
- ECR, IAM user, IAM OIDC provider: read then plain put; concurrent tag writes can lose a key.
  INFERRED.
- RDS, EKS, IAM role, policy and instance profile: revision-checked; code.

### Partial failure across resources in one call

- ELBv2 `AddTags` writes each ARN before resolving the next, so an unknown ARN after a valid one fails the call with the first resource already tagged.
  Test: `ELBv2AddTagsPartiallyAppliesBeforeUnknownARN`.
- EC2 `CreateTags`/`DeleteTags`: the central loop stops at the first failing resource after committing earlier ones; the daemon then writes the record mirrors only if the central write succeeded, and a mirror error is returned after the central index is already committed.
  Code (`daemon/daemon_handlers_tag.go:98`); the resulting index/record divergence is INFERRED.
- Instance IDs are tagged concurrently and the first error in input order is returned; other instances may already be tagged.
  Code (`daemon/daemon_handlers_tag.go:172`).

### Validation limits

- EC2: `aws:` refused at AWSGW with `InvalidParameterValue`; no count, key-length or value-length limit.
  Test: `EC2CreateTagsAppliesNoLengthOrCountLimits` (51 tags, a 129-character key and a 257-character value all stored).
- ELBv2, ECS, ACM: no count, length or `aws:` rule.
  Tests: `ELBv2AddTagsAppliesNoTagLimits`, `ECSTagResourceAppliesNoTagLimits`, `ACMAddTagsAppliesNoTagLimits`.
- ECR: empty key only; EKS: none.
  Code.
- RDS and IAM: AWS limits enforced.
  Code.

### Unknown, nonexistent and cross-account resources

- EC2: a well-formed ID that names nothing, an ID with no recognised prefix, and another account's ID are all accepted and stored under the caller's account.
  Test: `EC2CreateTagsStoresUnresolvedIDsInCallerNamespace` (extends `mulga-6maav`).
- `GetResources` then lists those IDs, an unrecognised one as `arn:aws:ec2:<region>:<account>:unknown/<id>`.
  Test: `GetResourcesListsUnresolvedEC2IDs`.
- ECS: an ARN naming another account and region resolves to the caller's same-named resource and tags it.
  Test: `ECSForeignAccountARNTagsCallerResource`.
- EKS and ECR discard the ARN account the same way.
  Code; the effect is INFERRED.
- ELBv2: unknown or foreign ARNs return the per-type not-found code; RDS returns `InvalidParameterValue` for a foreign account or region.
  Code.

### Error codes

| Owner | Unknown resource | Bad tag | Store failure |
| --- | --- | --- | --- |
| EC2 | none (accepted) | `ErrorInvalidParameterValue` (`aws:` and empty key only) | `ErrorServerInternal` |
| ELBv2 | `ErrorELBv2LoadBalancerNotFound`, `ErrorELBv2TargetGroupNotFound`, `ErrorELBv2ListenerNotFound`, `ErrorELBv2RuleNotFound` | `ErrorInvalidParameterValue` (empty key only) | `ErrorServerInternal` |
| RDS | the kind's not-found fault (`tags.go:60`) | `ErrorInvalidParameterValue` | error passed through |
| ECS | `ErrorECSClusterNotFound`, `ErrorECSServiceNotFound`, otherwise `ErrorECSInvalidParameter` | none | error passed through |
| EKS | `ErrorEKSResourceNotFound` for clusters (`service_impl.go:1958`); other ARNs `ErrorNotImplemented` | none | error passed through |
| ECR | `RepositoryNotFoundError` | `ErrorInvalidTagParameter` (empty key only) | error passed through |
| ACM | `ErrorACMInvalidArn` or `ErrorResourceNotFound` (`service_impl.go:614`; also when the certificate is deleted between the ownership check and the write) | none | `ErrorInternalError` |
| IAM | `ErrorIAMNoSuchEntity` | `validationError`, `ErrorIAMInvalidInput`, `ErrorIAMLimitExceeded` | error passed through |

Names are `awserrors` constants or package helpers.
This table is from code.

### GetResources during and after writes

- A resource whose tags were all removed drops out of the result.
  Test: `GetResourcesOmitsResourceWhoseTagsWereAllRemoved`.
- The pagination token is an integer offset into a set recomputed per call, so a resource tagged between page reads that sorts ahead of the offset makes the next page repeat an entry.
  Test: `GetResourcesOffsetTokenRepeatsAfterInterleavedWrite`.
  The same mechanism can skip an entry when a resource ahead of the offset is untagged (INFERRED).
- A non-integer token returns `InvalidParameterValue`, and `ResourcesPerPage` of 500 is accepted.
  Test: `GetResourcesPaginationValidation`.
- Only EC2 index entries and ELBv2 load balancers and target groups are aggregated; any other type filter returns an empty list without error.
  Test: `GetResourcesAggregatesOnlyEC2AndELBv2`.
- ELBv2 load balancers and target groups with no tags are included, because ELBv2 `DescribeTags` returns one entry per ARN.
  Code (`handlers/elbv2/service_impl.go:3041`).

### Authorization

- No path supplies resource tags, request tags or tag keys to the policy evaluator.
  Code.
- IAM `CreatePolicy` refuses policies conditioned on `aws:ResourceTag/`, `aws:RequestTag/`, `aws:TagKeys` and `ec2:ResourceTag/` with `MalformedPolicyDocument`.
  Test: `CreatePolicyRefusesTagConditionKeys` (links `mulga-5eh43`, `mulga-lmq4l`).

## 3. AWS parity

Each item states AWS behaviour first, then ours.
Sources were fetched on 2026-10-13.

- **Tag limits.**
  AWS: 50 tags per resource, 128-character keys, 256-character values, keys and values case-sensitive, `aws:` reserved ([EC2 tag restrictions](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/Using_Tags.html#tag-restrictions); [ECS TagResource](https://docs.aws.amazon.com/AmazonECS/latest/APIReference/API_TagResource.html); [EKS TagResource](https://docs.aws.amazon.com/eks/latest/APIReference/API_TagResource.html) gives 50 entries, key 1..128, value up to 256; [ECR TagResource](https://docs.aws.amazon.com/AmazonECR/latest/APIReference/API_TagResource.html) gives `TooManyTagsException` at 50 and `InvalidTagParameterException`; [ACM AddTagsToCertificate](https://docs.aws.amazon.com/acm/latest/APIReference/API_AddTagsToCertificate.html) gives 1..50 items, `InvalidTagException` for `aws:`, `TooManyTagsException`; [ELBv2 AddTags](https://docs.aws.amazon.com/elasticloadbalancing/latest/APIReference/API_AddTags.html) lists `TooManyTags` and `DuplicateTagKeys`).
  Ours: EC2, ELBv2, ECS, EKS, ECR and ACM do not enforce the count or lengths; ELBv2, ECS, EKS, ECR and ACM do not refuse `aws:`.
  Divergence, verified by the limit tests.
- **EC2 nonexistent or malformed IDs.**
  AWS: the error table defines `InvalidID` as "The specified ID for the resource you are trying to tag is not valid" ([EC2 error codes](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/errors-overview.html)).
  What AWS returns for a well-formed ID naming no resource on `CreateTags` is unverified.
  Ours: accepted and stored.
  The malformed-ID case is a divergence, verified by test.
- **EC2 cross-account IDs.**
  AWS: "When you tag public or shared resources, the tags you assign are available only to your AWS account" ([EC2 tag restrictions](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/Using_Tags.html#tag-restrictions)).
  AWS behaviour for an ID owned by another account and not shared with the caller is unverified.
  Ours stores under the caller in both cases, which matches the documented shared case and is unverified for the unshared case.
- **EC2 consistency.**
  AWS: the EC2 API "follows an eventual consistency model" ([EC2 error codes, Eventual consistency](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/errors-overview.html#api-eventual-consistency)).
  A lost tag write is not eventual consistency; the record-mirror revert is a defect regardless of this model.
- **Multi-resource atomicity.**
  AWS: Resource Groups Tagging `TagResources` reports per-ARN failures in `FailedResourcesMap` ([TagResources](https://docs.aws.amazon.com/resourcegroupstagging/latest/APIReference/API_TagResources.html)), so partial success is part of that contract.
  Whether ELBv2 `AddTags` or EC2 `CreateTags` is all-or-nothing across resources is unverified.
  Ours: both apply partially before failing.
- **GetResources content.**
  AWS: "`GetResources` does not return untagged resources", and without a tag filter "Resources that were previously tagged, but do not currently have tags, are shown with an empty tag set" ([GetResources](https://docs.aws.amazon.com/resourcegroupstagging/latest/APIReference/API_GetResources.html)).
  Ours drops previously tagged resources with no tags (test) and includes never-tagged ELBv2 load balancers and target groups (code).
  Both are divergences.
- **GetResources coverage.**
  AWS: "All tagged resources, whether the tagging used the Resource Groups Tagging API or not, are returned" (same page).
  Ours returns EC2 and two ELBv2 types only.
  Divergence, verified by test.
- **GetResources pagination.**
  AWS: `ResourcesPerPage` "minimum of 1 and a maximum value of 100"; invalid input is `InvalidParameterException`; a token is valid for at most 15 minutes, after which `PaginationTokenExpiredException` (same page).
  Ours accepts 500 and returns `InvalidParameterValue` for a bad token.
  Divergence, verified by test.
  AWS's page consistency under concurrent writes is unverified, so the repeat on interleaved writes is reported as current behaviour, not as a parity gap.
- **TagResources and UntagResources.**
  AWS: requires `tag:TagResources` and the owning service's tagging permission, for example `ec2:CreateTags`; at most 20 ARNs and 50 tags per call (TagResources page).
  Ours: not served (code).
- **Tag condition keys.**
  AWS: `aws:ResourceTag/<key>` compares the tag on the resource the request is made against; `aws:RequestTag/<key>` the tag passed in the request; `aws:TagKeys` the list of keys passed ([IAM global condition keys](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_condition-keys.html)).
  Ours: refused at policy write and never evaluated.
  Divergence, verified by test; tracked by `mulga-5eh43` and `mulga-lmq4l`.
- **ECS, EKS and ECR ARN with another account.**
  AWS error for that case is unverified.
  Ours acts on the caller's same-named resource (ECS by test, EKS and ECR by code).
- **ACM private key.**
  No AWS documentation addresses this; tagging is metadata only on every AWS page fetched.
  The key-loss defect is resolved (`mulga-ncn9f`).

## 4. Proposal: the ADR-0001 structural cut

ADR-0001 (accepted 2026-09-28) gives the rule this proposal works within: a consumer declares its own narrow in-process capability interface, an owner owns any versioned process contract under `contracts/<domain>/v1`, and a composition root wires them.
The ADR's package map row for `tags` sends generic helpers to `foundation/aws/tags` and says each resource owner retains canonical tag state.

### Scope

Decided: `domains/tagging` is a stateless aggregator composed in the AWSGW process, not a new daemon.
Decided: any owner wire contract belongs under that owner's `contracts/<owner>/v1`, and that there is no shared tagging wire format.

Inside the cut:

- `domains/tagging` owns the current `GetResources` aggregation path and nothing else.
  Today `gateway/tagging` serves only `GetResources` (`spinifex/spinifex/gateway/tagging.go:19`), so this is a structural move with no new API.
- `domains/tagging` declares one read/list capability, enough to reach the EC2 tag index and ELBv2 without importing either owner.
- The AWSGW composition root supplies one adapter per owner that implements that capability.
- `GetResources` behaviour is unchanged, including every behaviour recorded in [Findings](#findings).

Outside the cut, listed under [Future direction](#future-direction-outside-the-adr-0001-structural-cut): pagination semantics, supported-resource scope, tag limits, `aws:` handling, ever-tagged tracking, tag-policy evaluation (`ResourceTagReader`), and every mutation API (`TagResources`, `UntagResources` and the per-service tag writes).

### Capability

`domains/tagging` is the consumer, so it declares the interface:

```go
package tagging

// TaggedLister returns one owner's resources for GetResources aggregation.
type TaggedLister interface {
	ListTagged(ctx context.Context, accountID string, typeFilters map[string]bool) ([]TaggedResource, error)
}

type TaggedResource struct {
	ARN  string
	Tags map[string]string
}
```

`typeFilters` keeps today's meaning: the lower-cased `ResourceTypeFilters` set, where an entry may be a full `service:type` or a bare service (`gateway/tagging/GetResources.go:206`).
Each adapter applies it to the types it serves, as `listELBv2` and `listEC2` do now.
Tag filtering, ARN sorting and the offset token stay in the aggregator unchanged.
The interface carries no mutation, no limits and no ever-tagged flag; those belong to the future direction.

### Adapters and compile-time checks

The owners run in daemons and the aggregator runs in AWSGW.
A daemon-side owner cannot satisfy a Go interface declared in the AWSGW process, so the capability is implemented by an AWSGW-side NATS client adapter.

- **EC2 index adapter:** wraps today's `listEC2` path, `ec2tags.NewNATSTagsService(...).DescribeTags` plus `buildEC2Mappings` (`gateway/tagging/GetResources.go:95`).
  It is constructed with the region, since EC2 IDs become ARNs there.
- **ELBv2 adapter:** wraps today's `elbv2Resources` path, `DescribeLoadBalancers`, `DescribeTargetGroups` and batched `DescribeTags` through `handlers_elbv2.NewNATSELBv2Service` (`gateway/tagging/GetResources.go:106`).
- **Placement:** the adapters live under the AWSGW composition root, `runtime/roles/awsgw`, which the ratchet already names as the role allowed to wire domain registrations (`architecture/layering/rules_test.go:17`) and which already imports `handlers/*` and `domains/*` packages (`runtime/roles/awsgw/awsgw.go:28` to `:46`).
- **Compile-time checks:** `var _ tagging.TaggedLister = (*ec2IndexLister)(nil)` and `var _ tagging.TaggedLister = (*elbv2Lister)(nil)` sit beside the adapters in AWSGW.
  No owner package carries a check against a `domains/tagging` interface.
- **Wire contracts:** both adapters reuse subjects and clients that exist today, so the cut defines no new wire contract.
  The remote owner of each subject owns its request/reply contract and handler; the EC2 `ec2.DescribeTags` and ELBv2 `elbv2.Describe*` subjects are where that contract would be formalised, under `contracts/ec2/v1` and `contracts/elbv2/v1`, if a later step needs it.
- **Wiring:** `runtime/roles/awsgw` builds the aggregator with both adapters and hands it to the gateway, which today calls `gateway_tagging.GetResources` with its NATS connection (`gateway/tagging.go:21`).

### Ratchet rows removed

- `architecture/layering/allowlist_test.go:125`: `gateway/tagging` → `domains/ec2/tags`.
- `architecture/layering/allowlist_test.go:126`: `gateway/tagging` → `handlers/elbv2`.
- `architecture/layering/legacy_test.go:24`: `gateway/tagging` as a frozen legacy package.
- `docs/PACKAGE_BOUNDARY_MIGRATION.md:653` and `:673` record `gateway/tagging` as open debt; both rows are updated when it clears.

`domains/tagging` starts with no allowlist rows and imports only `foundation` packages and the AWS SDK model types it already uses.

### Migration order and exit criteria

1. **Interfaces in `domains/tagging`, read/list only.**
   Exit: `TaggedLister` and `TaggedResource` exist, the package imports no domain, handler or contract package, and the ratchet classifies it with zero allowlist rows.
2. **EC2 index and ELBv2 adapters in AWSGW.**
   Exit: both adapters compile with their `var _ tagging.TaggedLister` checks, each has a test against embedded JetStream equivalent to today's `listEC2` and `elbv2Resources`, and nothing calls them yet.
3. **Move `GetResources` aggregation from `gateway/tagging` to `domains/tagging`.**
   The request parsing, type and tag filtering, sorting, offset token and `authz.go` move; the gateway dispatch in `gateway/tagging.go` calls the aggregator wired by AWSGW.
   Exit: the `TestCurrentBehaviour_Tagging_GetResources*` tests and the existing `gateway/tagging` unit tests pass with unchanged assertions after moving with the code.
4. **Remove the ratchet rows.**
   Exit: `allowlist_test.go:125`, `:126` and `legacy_test.go:24` are deleted, the `gateway/tagging` package is gone, `make preflight`'s layering tests pass, and `docs/PACKAGE_BOUNDARY_MIGRATION.md:653` and `:673` record the debt as cleared.

The RDS and ECS record-writing edges at `docs/PACKAGE_BOUNDARY_MIGRATION.md:452` to `:454` are not cleared by this cut.
They need owner tag mutation capabilities, which are future direction.

## Future direction (outside the ADR-0001 structural cut)

Nothing in this section is part of the ADR-0001 track.
It records a possible shape for later work so the cut above does not block it.

### Owner tag mutation

A later `domains/tagging` could declare a mutation capability:

```go
type Tagger interface {
	ResourceTypes() []string // "service:type", e.g. "ec2:security-group"
	TagResource(ctx context.Context, accountID, arn string, add map[string]string) error
	UntagResource(ctx context.Context, accountID, arn string, keys []string) error
	ListTags(ctx context.Context, accountID, arn string) (map[string]string, error)
}
```

For owners in another process, the same model as the cut applies: the owner defines a request/reply contract under `contracts/<owner>/v1` and serves it, and an AWSGW-side adapter implements `Tagger` and carries the `var _ tagging.Tagger` check.
An owner in the same process as its consumer can implement the interface directly.

Errors would be sentinels declared in `domains/tagging` (`ErrNotFound`, `ErrInvalidARN`, `ErrInvalidTag`, `ErrTooManyTags`, `ErrConflict`), wrapping the owner's own `awserrors` code so per-service APIs keep today's codes.

Guarantees each owner would give:

1. Per-resource atomic: validate first, then one revision-checked write that changes only the tag field, retried on conflict with the mutation re-applied to the fresh record.
2. Existence and ownership: the ARN's partition, region and account must match the caller, and the resource must exist, before any write.
3. Limits from `foundation/aws/tags` with the owner's documented variations.
4. A tag write never rewrites a non-tag field.

No owner would be asked for cross-resource atomicity.

### Work that would use it

- `TagResources` and `UntagResources` in `domains/tagging`, which are an API expansion, reporting per-ARN failures in `FailedResourcesMap`.
- Clearing the RDS subnet-group and parameter-group edges and the ECS task-definition edge (`docs/PACKAGE_BOUNDARY_MIGRATION.md:452` to `:454`) by calling owner mutation capabilities.
- ACM, EKS, ECR and IAM owners; ACM's key-loss defect is already fixed (`mulga-ncn9f`).
- EC2 record owners, once the canonical EC2 tag store is decided (`mulga-36wn6`, `mulga-6maav`).
- `foundation/aws/tags` validation helpers shared by every owner.
- `GetResources` pagination, supported-resource scope and ever-tagged tracking (`mulga-t6cuv`).

### Tag-policy evaluation

The policy door would declare its own read-only capability:

```go
type ResourceTagReader interface {
	ResourceTags(ctx context.Context, accountID, arn string) (map[string]string, bool, error)
}
```

`aws:RequestTag` and `aws:TagKeys` come from the parsed request and need no owner call.
This depends on `mulga-lmq4l` and `mulga-5eh43`.

## Open decisions

These need an owner's decision and are not settled by this document.
None of them blocks the ADR-0001 cut.

1. **EC2 canonical store.** Whether the `ec2-tags` index becomes the canonical store for every EC2 type, or records stay canonical and the index becomes a projection each owner maintains.
2. **Pagination token format and lifetime.** Keyset token versus snapshot, and whether to mirror AWS's 15-minute expiry.
3. **Cross-account ARN behaviour** for ECS, EKS and ECR, given that AWS behaviour for these is unverified here.
4. **Ever-tagged tracking.** Whether owners must remember that a resource was once tagged, to match `GetResources` returning `"Tags": []`.
5. **Which `TagResources` permission check runs where.** The gateway door, the aggregator, or each owner.
6. **Internal callers and `aws:` keys.** EC2 refuses `aws:` only at AWSGW; whether internal writers (system VPC, EKS controllers) may keep writing reserved keys through a mutation capability.

Decided by the user and removed from this list: where `domains/tagging` runs (AWSGW, stateless) and the process contract shape (owner contracts only, no shared format).

## Findings

Current behaviour only; none is fixed except finding 1, marked resolved.

| # | Finding | Evidence | Bead |
| --- | --- | --- | --- |
| 1 | Resolved: ACM tag writes erased the stored private key; they now preserve it. Certificates tagged before the fix may have lost their key and need re-import or re-issuance. | Test: `ACMTagWritesPreserveStoredPrivateKey` | `mulga-ncn9f` (P1, closed); remediation `mulga-o436p` (P1, open) |
| 2 | EC2 record mirrors revert concurrent tag and non-tag writes. | Test: `RecordMirrorRevertsInterleavedWrites` | `mulga-36wn6` (P1) |
| 3 | ECS tag writes revert concurrent record writes, including a `DesiredCount` change. | Test: `ECSTagWriteRevertsInterleavedRecordWrite` | `mulga-c0d5x` (P1) |
| 4 | ECS acts on the caller's resource for an ARN naming another account. | Test: `ECSForeignAccountARNTagsCallerResource`; EKS and ECR INFERRED | `mulga-k8d9b` (P2) |
| 5 | EC2 stores tags for unresolved and other-account IDs. | Test: `EC2CreateTagsStoresUnresolvedIDsInCallerNamespace` | `mulga-6maav` |
| 6 | EC2, ELBv2, ECS and ACM enforce no tag count or length limits. | Tests: `EC2CreateTagsAppliesNoLengthOrCountLimits`, `ELBv2AddTagsAppliesNoTagLimits`, `ECSTagResourceAppliesNoTagLimits`, `ACMAddTagsAppliesNoTagLimits`; EKS and ECR code | none |
| 7 | ELBv2 `AddTags` partially applies before an unknown ARN. | Test: `ELBv2AddTagsPartiallyAppliesBeforeUnknownARN` | `mulga-liyz5` (P2) |
| 8 | `GetResources` lists unresolved EC2 IDs, including type `unknown`. | Test: `GetResourcesListsUnresolvedEC2IDs` | `mulga-t6cuv` (P2), `mulga-6maav` |
| 9 | `GetResources` drops resources whose tags were all removed. | Test: `GetResourcesOmitsResourceWhoseTagsWereAllRemoved` | `mulga-t6cuv` (P2) |
| 10 | `GetResources` offset token repeats an entry after an interleaved write. | Test: `GetResourcesOffsetTokenRepeatsAfterInterleavedWrite` | `mulga-t6cuv` (P2) |
| 11 | `GetResources` accepts page sizes over 100 and returns `InvalidParameterValue` for a bad token. | Test: `GetResourcesPaginationValidation` | `mulga-t6cuv` (P2) |
| 12 | `GetResources` covers only EC2 and ELBv2 load balancers and target groups. | Test: `GetResourcesAggregatesOnlyEC2AndELBv2` | `mulga-t6cuv` (P2) |
| 13 | `GetResources` includes untagged ELBv2 load balancers and target groups. | Code | `mulga-t6cuv` (P2) |
| 14 | `TagResources`, `UntagResources`, `GetTagKeys`, `GetTagValues` are not served. | Code | none |
| 15 | Tag condition keys are refused at policy write and never evaluated. | Test: `CreatePolicyRefusesTagConditionKeys` | `mulga-5eh43`, `mulga-lmq4l` |
| 16 | ELBv2 target group tag CAS merges from a stale read; LB, listener and rule tag writes are whole-record puts. | INFERRED | none |
| 17 | ECR, IAM user and IAM OIDC provider tag writes are read then plain put. | INFERRED | none |
| 18 | The EC2 daemon writes the central index before record mirrors, and a mirror failure leaves them divergent. | Code; divergence INFERRED | none |
| 19 | Creation-time tags reach the central index only for instance, vpc/subnet/sg/eni and key pair. | Code; matches `creation-time-tags-describetags.md` | none |
| 20 | `launchtemplate` and `placementgroup` tag writes reach only the central index. | Code | none |

`mulga-t6cuv` groups the `GetResources` parity gaps; whether it covers findings 8 and 13 is for its owner to confirm.

## Tests

Each asserts current behaviour:

| Test | File |
| --- | --- |
| `TestCurrentBehaviour_Tagging_RecordMirrorRevertsInterleavedWrites` | `spinifex/spinifex/domains/ec2/tagmirror/tagging_current_behaviour_test.go` |
| `TestCurrentBehaviour_Tagging_EC2CreateTagsStoresUnresolvedIDsInCallerNamespace` | `spinifex/spinifex/daemon/tagging_current_behaviour_test.go` |
| `TestCurrentBehaviour_Tagging_EC2CreateTagsAppliesNoLengthOrCountLimits` | `spinifex/spinifex/gateway/tagging_current_behaviour_test.go` |
| `TestCurrentBehaviour_Tagging_GetResourcesListsUnresolvedEC2IDs` | same |
| `TestCurrentBehaviour_Tagging_GetResourcesOmitsResourceWhoseTagsWereAllRemoved` | same |
| `TestCurrentBehaviour_Tagging_GetResourcesOffsetTokenRepeatsAfterInterleavedWrite` | same |
| `TestCurrentBehaviour_Tagging_GetResourcesPaginationValidation` | same |
| `TestCurrentBehaviour_Tagging_GetResourcesAggregatesOnlyEC2AndELBv2` | same |
| `TestCurrentBehaviour_Tagging_ELBv2AddTagsPartiallyAppliesBeforeUnknownARN` | `spinifex/spinifex/handlers/elbv2/tagging_current_behaviour_test.go` |
| `TestCurrentBehaviour_Tagging_ELBv2AddTagsAppliesNoTagLimits` | same |
| `TestCurrentBehaviour_Tagging_ECSTagWriteRevertsInterleavedRecordWrite` | `spinifex/spinifex/handlers/ecs/tagging_current_behaviour_test.go` |
| `TestCurrentBehaviour_Tagging_ECSForeignAccountARNTagsCallerResource` | same |
| `TestCurrentBehaviour_Tagging_ECSTagResourceAppliesNoTagLimits` | same |
| `TestCurrentBehaviour_Tagging_ACMTagWritesPreserveStoredPrivateKey` | `spinifex/spinifex/domains/acm/tagging_current_behaviour_test.go` |
| `TestCurrentBehaviour_Tagging_ACMAddTagsAppliesNoTagLimits` | same |
| `TestCurrentBehaviour_Tagging_CreatePolicyRefusesTagConditionKeys` | `spinifex/spinifex/handlers/iam/tagging_current_behaviour_test.go` |

The gateway tests serve the EC2 tag subjects from a real `TagsServiceImpl` without the daemon's record-mirror fan-out, so they cover the central index only.
The interleaving tests wrap the KV handle to run one write between the handler's read and its write; they show the window exists, not how often real nodes hit it.
