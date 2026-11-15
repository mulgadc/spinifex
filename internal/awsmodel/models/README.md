# AWS models

The `*.json.gz` files are the Smithy JSON AST service models from [`aws/api-models-aws`](https://github.com/aws/api-models-aws), gzipped and embedded into the binary, so loading them needs no network or Go module cache. All eleven come from the one commit recorded in `../model_source.go`.

Do not edit them by hand. To move to a newer commit, run `scripts/sync-aws-models.sh <commit-sha>`, then `make generate-aws-model-coverage` and review the regenerated `docs/coverage` and the `awsmodel` test expectations: a newer model can add operations, required members and enum values, and each resulting finding is a real gap to triage rather than a reason to stay on an older commit.

`ec2/error-codes.json` is separate from the service models because the EC2 model declares no operation errors. It is a curated subset of the official [EC2 error-code reference](https://docs.aws.amazon.com/ec2/latest/devguide/errors-overview.html): all documented common and server codes, plus the action-specific codes that Spinifex currently emits. The catalog records its verification date and must be reviewed against that source when it changes.

`missing` in the generated coverage is not a task list, and `requires_sdk_v2` is the part of the model aws-sdk-go v1 (Spinifex's frozen SDK) cannot call, derived from the SDK itself. Never register or stub an operation without a real handler and a bead. An operation under `requires_sdk_v2` is out of scope until the SDK v2 migration. An unregistered operation already returns the proper unsupported error, which beats a stub that fakes success. `TestNoStubsForOperationsAbsentFromSDKV1` in `cmd/aws-model-coverage` fails on a stub or refusal for an operation absent from SDK v1; fix it by deleting the registration.
