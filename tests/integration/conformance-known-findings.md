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

The same baseline examined 42 EC2 error responses and exposed 16 violations. The
`InsufficientInstanceCapacity` status-class defect has since been fixed — that
code now returns 503, matching the other capacity errors — leaving 15 open:

| Finding | Count | Classification | Implementation cause |
|---|---:|---|---|
| EC2 authorization failures return `AccessDenied`, which is absent from the EC2 catalog; EC2 documents `UnauthorizedOperation` | 15 | Defect | The shared policy evaluator returns the Query/IAM-oriented `AccessDenied` code for EC2 requests. |

Six additional production-referenced values are deliberately not accepted by the catalog because the chosen EC2 reference does not list them: `ExpiredToken`, `IamInstanceProfileAlreadyAssociated`, `InvalidIamInstanceProfile.NotFound`, `NoSuchAssociation`, `NoSuchEntity`, and `RequestEntityTooLarge`. A runtime occurrence remains a visible violation until it is either backed by an authoritative EC2 source or replaced with a documented EC2 code.

## Operation coverage

Phase 5 generates the public model-versus-dispatch inventory as one page per service under `docs/coverage/`, plus an index carrying the cross-service summary. Run `make aws-model-coverage` to regenerate the pages and print a terminal summary. A normal `make build` and the integration target both regenerate the pages so handler-map changes cannot silently leave them stale.

The inventory counts modelled operations bound to real handlers separately from registered stubs and deliberately unsupported handlers. S3 is marked opaque because Spinifex delegates its REST surface to Predastore rather than using an operation-name dispatch table; no mechanical S3 coverage percentage is claimed.

## Request conformance

`TestRequestConformance` generates requests from the service models for every implemented operation except S3's, sends them through the in-process gateway with every daemon-lite wired, and judges each response. An acceptance request carries the required members plus at most one optional member and should not be refused as invalid. A rejection request breaks one constraint (a required member, enum, length, range or pattern) and should be refused with a validation error. Anything else, such as a missing resource, is inconclusive.

Request findings block only for services listed under `promotedRequestServices` in `conformance-promoted-services.json`, which starts empty; response promotion does not carry over.

The 2026-09-28 baseline sent 3968 requests across 368 operations: 898 pass, 385 findings, 2685 inconclusive. None of it has been checked against AWS, so each finding below is a lead to verify before any fix.

| Finding | Count | Notes |
|---|---:|---|
| A request breaking a model constraint is accepted | 322 | IAM 116, ECS 51, ELBv2 48, ECR 41, EC2 34, ACM 15, RDS 7, EKS 6, STS 4. Examples: IAM `List*` accept out-of-range `MaxItems` and `Marker`; IAM, ELBv2 and ECR accept a tag without `Key`; EC2 `Describe*` accept out-of-range `MaxResults`; ECS `DescribeServices` accepts no `services`. |
| A model-valid request is refused as invalid | 63 | Mostly deliberate limitations that say so: RDS `ModifyDBInstance` members, STS session policies, tags and MFA, IAM `PermissionsBoundary`, ECS `availabilityZoneRebalancing`. Others return a generic message whose cause is unresolved (ECS, EC2, EKS), or the model leaves out a value AWS itself requires, such as an RDS `AllocatedStorage` below the engine minimum. |

Known limits of the sweep:

- Models mark conditionally required members optional, so an acceptance request refused with `MissingParameter` or `InvalidParameterCombination` is inconclusive, as is every later request for that operation once its required-only request is refused.
- Filter names and pagination tokens cannot be generated validly, so `Filters`, `Marker` and `NextToken` get rejection requests only.
- ECR and EKS refuse invalid input with `InvalidParameterValueException`, which neither model declares; both declare `InvalidParameterException`. The sweep counts it as a validation refusal.
- Most inconclusives are resource lookups that run before validation (`NoSuchEntity`, `*NotFound`), EC2 fan-outs no daemon-lite answers (`InternalError`), and EKS operations whose backends are absent (`ServiceUnavailableException`, `NotImplementedException`).
- The EC2 query parser ignores parameters it does not know, which no generated request can reveal because every member it sends is modelled.
