# AWS models

The `*.json.gz` files are the Smithy JSON AST service models from [`aws/api-models-aws`](https://github.com/aws/api-models-aws), gzipped and embedded into the binary, so loading them needs no network or Go module cache. All ten come from the one commit recorded in `../model_source.go`.

Do not edit them by hand. To move to a newer commit, run `scripts/sync-aws-models.sh <commit-sha>`, then `make generate-aws-model-coverage` and review the regenerated `docs/coverage` and the `awsmodel` test expectations: a newer model can add operations, required members and enum values, and each resulting finding is a real gap to triage rather than a reason to stay on an older commit.

`ec2/error-codes.json` is separate from the service models because the EC2 model declares no operation errors. It is a curated subset of the official [EC2 error-code reference](https://docs.aws.amazon.com/ec2/latest/devguide/errors-overview.html): all documented common and server codes, plus the action-specific codes that Spinifex currently emits. The catalog records its verification date and must be reviewed against that source when it changes.
