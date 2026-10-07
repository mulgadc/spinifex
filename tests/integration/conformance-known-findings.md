# AWS model conformance ratchet

The integration suite validates every successful modelled AWS response. Services listed in `conformance-promoted-services.json` are blocking in the default `fail` mode; findings from all other services remain warnings until triaged and fixed.

Run the ratchet with:

```sh
make test-integration
```

Use `AWS_MODEL_CONFORMANCE_MODE=warn` only for investigation. The report is written to `.cache/aws-model-conformance-report.txt` by default.
## Error-code coverage

Phase 4 validates IAM, STS, ECS, and ELBv2 error envelopes against their service models. The 2026-08-05 full integration baseline checked 46 operation-declared errors (45 IAM, one STS) with no error-code violations. ECS and ELBv2 decoding/model comparison have focused tests but their error paths are not exercised by the current integration suite, so neither service is ready for promotion on that evidence.

Twelve responses are reported separately as `errors_unmodelled`: four IAM and eight STS. These are protocol-level authentication/authorization/signature/ throttling errors that AWS omits from operation error lists, plus STS's runtime `ValidationError` and `InvalidParameterValue` responses. “Unmodelled” is not a conformance pass; it means the operation model cannot judge the code. They stay visible in the report rather than being silently counted as conforming.

EC2's model declares no operation errors, so EC2 uses a separate checked-in catalog curated from AWS's official EC2 error-code reference. It contains every documented common and server error plus the documented action-specific codes Spinifex currently emits. The validator checks the EC2 XML envelope, catalog membership, and AWS's documented 4xx client / 5xx server classification.

The same baseline examined 42 EC2 error responses and exposed 16 violations. The `InsufficientInstanceCapacity` status-class defect has since been fixed — that code now returns 503, matching the other capacity errors. The 2026-09-29 run examined 50 EC2 error responses and found 23 violations, all EC2 policy denials answered with the IAM-oriented `AccessDenied`. EC2 now answers a policy denial, including an `iam:PassRole` denial, with `UnauthorizedOperation`, and a rerun of the same 50 responses reports no violations.

Six additional production-referenced values are deliberately not accepted by the catalog because the chosen EC2 reference does not list them: `ExpiredToken`, `IamInstanceProfileAlreadyAssociated`, `InvalidIamInstanceProfile.NotFound`, `NoSuchAssociation`, `NoSuchEntity`, and `RequestEntityTooLarge`. A runtime occurrence remains a visible violation until it is either backed by an authoritative EC2 source or replaced with a documented EC2 code.

## Operation coverage

Phase 5 generates the public model-versus-dispatch inventory as one page per service under `docs/coverage/`, plus an index carrying the cross-service summary. Run `make aws-model-coverage` to regenerate the pages and print a terminal summary. A normal `make build` and the integration target both regenerate the pages so handler-map changes cannot silently leave them stale.

The inventory counts modelled operations bound to real handlers separately from registered stubs and deliberately unsupported handlers. S3 is marked opaque because Spinifex delegates its REST surface to Predastore rather than using an operation-name dispatch table; no mechanical S3 coverage percentage is claimed.

## Request conformance

`TestRequestConformance` generates requests from the service models for every implemented operation except S3's, sends them through the in-process gateway with every daemon-lite wired, and judges each response. An acceptance request carries the required members plus at most one optional member and should not be refused as invalid. A rejection request breaks one constraint (a required member, enum, length, range or pattern, including a map key's) and should be refused with a validation error. Anything else, such as a missing resource, is inconclusive.

A rejection that succeeds is always a finding. A refusal counts as a pass only when the acceptance request it builds on was accepted, and only when its code is one the operation declares or one AWS lists as common to every service (`ValidationError`, `ValidationException`, `MissingParameter`, `InvalidParameterValue`, `InvalidParameterCombination`). Any other validation code is reported as an undeclared error. Operations that declare no errors, which includes all of EC2, are not compared.

Request findings and undeclared errors block only for services listed under `promotedRequestServices` in `conformance-promoted-services.json`, which starts empty; response promotion does not carry over.

The 2026-09-29 baseline sent 4047 requests across 371 operations: 980 pass, 506 findings, 96 undeclared errors, 2465 inconclusive. None of it had been checked against AWS, so each finding below is a lead to verify before any fix. Since then, EC2 range-checks `MaxResults` on six `Describe*` calls and refuses a tag resource type the operation cannot tag, as AWS answers both, which clears 12 EC2 findings. IAM tag and untag calls now refuse a tag with no value or outside the model's key and value character sets, count tag lengths in characters rather than bytes, treat keys that differ only in case as one key on users and roles but as two elsewhere, and refuse the reserved `aws:` prefix, each as AWS answered it. IAM also checks tags on every create, `PathPrefix` on every list call (and `ListAttached*Policies` now filters by it), the `Scope` and `PolicyUsageFilter` enums, role and policy `Description` length, role `MaxSessionDuration` against 3600–43200, OIDC client ID and thumbprint lengths, `ListEntitiesForPolicy` `PolicyArn` length and an empty `CreatePolicy` `Path`, and answers name and path length breaks with `ValidationError`, each verified against AWS. STS `GetSessionToken` refuses an out-of-range `DurationSeconds` rather than clamping it, as AWS answers `AssumeRole`'s, and both calls check `SerialNumber` and `TokenCode` against the model's length and pattern before refusing MFA. Every finding is traced to its cause, and grouped into fix items, in the mulga plan `docs/development/archive/bugs/aws-integration-conformance-findings.md`.

| Finding | Count | Notes |
|---|---:|---|
| A request breaking a model constraint is accepted | 420 | ECS 116, IAM 112, ACM 56, ELBv2 48, ECR 47, EC2 22, RDS 7, STS 6, EKS 6. Examples: ACM and ELBv2 accept a tag without `Key`, as IAM did; EC2 network `Describe*` accepted out-of-range `MaxResults`; ECS `CreateCapacityProvider` accepts every enum and range break inside `autoScalingGroupProvider` and `managedInstancesProvider`; ACM `RequestCertificate` accepts every break inside `DomainValidationOptions`; STS `GetSessionToken` accepted out-of-range `DurationSeconds` and a `SerialNumber` and `TokenCode` below their minimum length, and still accepts an out-of-range `MinimumSessionTokenSize`. |
| A model-valid request is refused as invalid | 74 | RDS 42, ECS 10, STS 10, EC2 8, EKS 2, IAM 2. Mostly deliberate limitations that say so: RDS `CreateDBInstance`, `ModifyDBInstance` and `RestoreDBInstanceFromDBSnapshot` members, STS session policies, tags and MFA, ECS `availabilityZoneRebalancing`. Others return a generic message whose cause is unresolved (ECS task and task-definition operations, EC2 `Modify*Attribute`), or add a member next to a seeded one that AWS may refuse in combination, such as EC2 `CreateCapacityReservation` `AvailabilityZoneId` beside `AvailabilityZone`. |
| A refusal uses an undeclared error code | 96 | EKS (2) answers with `InvalidParameterValueException`; its model declares `InvalidParameterException`. ECR (90) answered with `InvalidParameterValueException` too; it now answers `InvalidParameterException`, as AWS's `CreateRepository` does for a name that breaks the pattern. IAM (4) answered `CreateGroup` and `Put*Policy` name-length breaks with `InvalidInput`, which those operations do not declare; they now answer `ValidationError`, as AWS does. |

How the sweep reaches its targets:

- Models mark conditionally required members optional. The members AWS requires anyway are seeded into every request for the operations listed in `conditionallyRequired` (`internal/awsmodel/request_hints.go`). For any other operation, an acceptance request refused with `MissingParameter` or `InvalidParameterCombination` is inconclusive, and so is every later refusal for that operation once its required-only request is refused.
- Members whose model shape accepts values AWS refuses, such as ARNs, IDs, zones, policy documents, certificates and ECR manifests, take hinted values of the right form (`request_hints.go`). An EC2 create's `TagSpecifications` names the resource type that operation creates, since AWS refuses any other.
- A pattern Go's regexp cannot compile, because it uses lookahead, is sampled through a hand-written RE2 equivalent (`re2Equivalents` in `internal/awsmodel/pattern.go`). An operation whose required member has a pattern with no equivalent is listed as `UNTESTED`; none is today.
- The runner creates an IAM role, managed policy, OIDC provider, instance profile, group and user, an ECS cluster and an ECR repository per run, and every operation except a create or delete names them. Other services have no fixture, so their tag, update and describe cases still end in not-found.

Known limits of the sweep:

- Filter names and pagination tokens cannot be generated validly, so `Filters`, `Marker` and `NextToken` get rejection requests only, and a refusal of one is inconclusive.
- Constraints no request breaks are counted as `unbroken_constraints`: required URI labels, maximum lengths above the generated limit (such as IAM policy documents), map entry counts, and patterns no value of a valid length breaks.
- Most inconclusives are resource lookups that run before validation (`NoSuchEntity`, `*NotFound`), ECS's bare `InvalidParameterException` for a missing resource, EC2 requests missing a conditionally required member (`MissingParameter`), EC2 subjects no daemon-lite answers (`InternalError`), RDS `ServerInternal`, and EKS operations whose backends are absent (`ServiceUnavailableException`, `NotImplementedException`).
- The EC2 query parser ignores parameters it does not know, which no generated request can reveal because every member it sends is modelled.
