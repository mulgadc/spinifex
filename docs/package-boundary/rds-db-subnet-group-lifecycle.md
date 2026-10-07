# RDS DB subnet group: lifecycle contract

Present contract under ADR-0003 S1, recorded before the resource moves.
It describes the code as it stands, so it claims no conformance; the gaps below are observed behaviour, not targets.
Paths are relative to `spinifex/` unless they start with `tests/`.

## Scope

A DB subnet group has no realization.
Create, modify and delete each complete synchronously within the request, and nothing outside the record has to be provisioned or cleaned up.
The group's one external effect is indirect: instance create and restore read it to choose the endpoint subnet.

## Contract

| Item | Present behaviour | Evidence |
|---|---|---|
| Identity | `DBSubnetGroupName`, case-sensitive, immutable, `default` reserved. ARN `arn:aws:rds:{region}:{account}:subgrp:{name}`. | `handlers/rds/subnetgroup_test.go` `TestCreateDBSubnetGroup_RejectsMalformedRequests`, `handlers/rds/arn_test.go` |
| Scope | Account and Region: one record per name in the `rds-account-{account}` bucket. Not AZ-scoped; each member subnet carries the AZ recorded on the EC2 subnet. | `TestDBSubnetGroup_NameIsScopedToTheAccount`, `TestCreateDBSubnetGroup_AcceptsASingleSubnet` |
| Durable owner and record | `handlers/rds/subnetgroup.go` owns `db-subnet-groups/{name}` (`DBSubnetGroupRecord`). `tags.go` also writes the record's `Tags` field through CAS. | `TestDBSubnetGroupRecord_PersistedFieldNames`, `TestDBSubnetGroup_TagsAreReachedThroughItsARN` |
| Desired state | Description, member subnets and their single VPC, and tags. Subnets are validated as the caller through EC2 `DescribeSubnets`: same account, one VPC, at most 20, no duplicates. | `TestCreateDBSubnetGroup_Rejects*`, `TestModifyDBSubnetGroup_RefusesAMoveToAnotherVPC` |
| Generation | None. Create is exclusive (`createJSON`); modify and tag writes retry against the KV revision so a concurrent write is not undone; delete is unconditional. | `TestCreateDBSubnetGroup_RejectsADuplicateName`, `TestTagWrites_ConcurrentAddsAndRemovesDoNotLoseEachOther` |
| Observed state | Not tracked separately. `SubnetGroupStatus` is always `Complete` and every member subnet reports `Active`. | `TestDescribeDBInstances_ReportsTheSubnetGroupTheInstanceWasPlacedFrom` |
| Readiness | Usable as soon as the create returns. | `TestCreateDBInstance_PlacesTheEndpointFromTheNamedSubnetGroup` |
| Idempotency | No client token, as in AWS. A repeated create returns `DBSubnetGroupAlreadyExists`; a repeated delete returns `DBSubnetGroupNotFoundFault`. Internal redelivery of a NATS request re-runs the same checks and gets the same answer. | `TestCreateDBSubnetGroup_RejectsADuplicateName`, `TestDeleteDBSubnetGroup_RepeatReportsNotFound` |
| Dependants | Instance create and restore read the group once, at placement, and store the chosen subnet and VPC on the instance. A later modify never moves a placed instance. | `TestModifyDBSubnetGroup_LeavesAPlacedInstanceWhereItIs`, `TestRestoreDBInstanceFromDBSnapshot_RejectsAnUnknownSubnetGroup`, `TestCreateDBInstance_RejectsAnUnknownSubnetGroup` |
| Deletion ordering | Refused with `InvalidDBSubnetGroupStateFault` while any instance record names the group, including a `deleting` one. Snapshots naming the group do not block it. Account teardown deletes instances at `StageCompute` and member EC2 subnets at `StageNetwork`, both before groups at `StagePlatform` (`accountteardown/reapers_rds.go`, `accountteardown/types.go`). | `TestDeleteDBSubnetGroup_RefusesWhileAnInstanceReferencesIt`, `TestDeleteDBSubnetGroup_IsNotBlockedByASnapshotNamingIt` |
| Restart and interruption | Each action is one KV write, so an interrupted request leaves either the old record or the new one; there is no partial cleanup to resume. | none beyond the single-write code path |
| End-to-end | `tests/e2e/rds/groups_test.go` `TestSubnetAndParameterGroups`; also exercised by `TestConnectivity`, `TestCrossSubnetConnectivity`, `TestSnapshotRestore` and `TestAPIMatrix`. No Terraform apply/destroy evidence for `aws_db_subnet_group`. | |

## Gaps

1. Delete is check-then-delete with no fence against a concurrent instance create or restore that has already read the group (`subnetgroup.go` `DeleteDBSubnetGroup`, `network.go` `resolvePlacement`), so an instance can name a deleted group. Its placement is already stored, so the endpoint keeps working; only the name dangles.
2. The in-use guard reads the instance's private records directly (`instancesUsingGroup`), and placement reads the group's private record directly (`getDBSubnetGroup` in `network.go`). Both cross the resource boundary ADR-0003 S4 forbids.
3. `DescribeDBInstances` projects the group from the instance record alone, so its `DBSubnetGroup` carries no description or subnets (`describe.go`). AWS documents both fields on that response; not verified against live AWS.
4. Member subnets always report `Active`, even after the EC2 subnet is deleted. What AWS reports in that case is not verified.
5. Modify accepts a new subnet set that drops the subnet a placed instance sits in. Whether AWS refuses this is not verified.
6. No ADR-0003 S5 failure or recovery evidence, and no Terraform apply/destroy evidence.
