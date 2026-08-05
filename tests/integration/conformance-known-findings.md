# AWS model conformance ratchet

The integration suite validates every successful modelled AWS response. Services
listed in `conformance-promoted-services.json` are blocking in the default
`fail` mode; findings from all other services remain warnings until triaged and
fixed.

Run the ratchet with:

```sh
make test-integration
```

Use `AWS_MODEL_CONFORMANCE_MODE=warn` only for investigation. The report is
written to `.cache/aws-model-conformance-report.txt` by default.

## Promoted services

- `sts` — promoted on 2026-08-05 after the integration baseline completed with
  no STS violations or decoder errors.

## Triaged warnings

Baseline from 2026-08-05: 370 successful responses checked, three violations,
zero decoder errors.

| Service / operation | Finding | Classification | Implementation cause |
|---|---|---|---|
| EC2 `DescribeImages` | `BootMode` is emitted as `""`, outside the model enum | Defect | Legacy AMI metadata is intentionally projected through `aws.String(amiMeta.BootMode)`, which emits an empty element instead of omitting the optional member. |
| IAM `ListAccessKeys` | Required `AccessKeyMetadata` missing | Defect | The empty result starts as a nil slice, so Query XML serialization omits the required list container. |
| IAM `ListGroups` | Required `Groups` missing | Defect | When the KV bucket has keys but none match the account, the result slice remains nil and Query XML serialization omits the required list container. |

These warnings are not suppressed by a finding allowlist. They remain visible
on every run. Handler changes belong in focused follow-up work; once a service's
warnings are resolved, add that service to the promoted-services file in the
same change that proves its full integration baseline is clean.

## Error-code coverage

Phase 4 validates IAM, STS, ECS, and ELBv2 error envelopes against their service
models. The 2026-08-05 full integration baseline checked 46 operation-declared
errors (45 IAM, one STS) with no error-code violations. ECS and ELBv2
decoding/model comparison have focused tests but their error paths are not
exercised by the current integration suite, so neither service is ready for
promotion on that evidence.

Twelve responses are reported separately as `errors_unmodelled`: four IAM and
eight STS. These are protocol-level authentication/authorization/signature/
throttling errors that AWS omits from operation error lists, plus STS's runtime
`ValidationError` and `InvalidParameterValue` responses. “Unmodelled” is not a
conformance pass; it means the operation model cannot judge the code. They stay
visible in the report rather than being silently counted as conforming.

EC2's model declares no operation errors, so EC2 uses a separate checked-in
catalog curated from AWS's official EC2 error-code reference. It contains every
documented common and server error plus the documented action-specific codes
Spinifex currently emits. The validator checks the EC2 XML envelope, catalog
membership, and AWS's documented 4xx client / 5xx server classification.

The same baseline examined 42 EC2 error responses and exposed 16 violations:

| Finding | Count | Classification | Implementation cause |
|---|---:|---|---|
| EC2 authorization failures return `AccessDenied`, which is absent from the EC2 catalog; EC2 documents `UnauthorizedOperation` | 15 | Defect | The shared policy evaluator returns the Query/IAM-oriented `AccessDenied` code for EC2 requests. |
| `RequestSpotInstances` returns `InsufficientInstanceCapacity` with HTTP 400 instead of the documented server-error 5xx class | 1 | Defect | The global error lookup currently assigns HTTP 400 to `InsufficientInstanceCapacity`. |

Six additional production-referenced values are deliberately not accepted by
the catalog because the chosen EC2 reference does not list them: `ExpiredToken`,
`IamInstanceProfileAlreadyAssociated`, `InvalidIamInstanceProfile.NotFound`,
`NoSuchAssociation`, `NoSuchEntity`, and `RequestEntityTooLarge`. A runtime
occurrence remains a visible violation until it is either backed by an
authoritative EC2 source or replaced with a documented EC2 code.

## Operation coverage

Phase 5 generates the public model-versus-dispatch inventory at
`docs/aws-model-operation-coverage.md`. Run `make aws-model-coverage` to
regenerate and print it. A normal `make build` and the integration target both
regenerate the document so handler-map changes cannot silently leave it stale.

The inventory counts modelled operations bound to real handlers separately
from registered stubs and deliberately unsupported handlers. S3 is marked
opaque because Spinifex delegates its REST surface to Predastore rather than
using an operation-name dispatch table; no mechanical S3 coverage percentage
is claimed.
