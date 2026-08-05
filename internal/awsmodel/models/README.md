# AWS model cache

The conformance suite reads `api-2.json` from the Go module cache for the
pinned `github.com/aws/aws-sdk-go` version `v1.55.8`. Running Go tests or
`go mod download` populates that cache; the large upstream model files are
intentionally not tracked in this repository.

`ec2/error-codes.json` is separate from the SDK models because EC2's
`api-2.json` declares no operation errors. It is a curated subset of the
official [EC2 error-code reference](https://docs.aws.amazon.com/ec2/latest/devguide/errors-overview.html):
all documented common and server codes, plus the action-specific codes that
Spinifex currently emits. The catalog records its verification date and must be
reviewed against that source when it changes.
