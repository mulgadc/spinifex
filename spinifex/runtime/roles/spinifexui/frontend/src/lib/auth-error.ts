// AWS error codes that indicate the credentials themselves are stale or
// invalid (authentication failures), as opposed to authorization denials
// (AccessDenied / UnauthorizedOperation) where valid creds lack permission.
const AUTH_ERROR_CODES = new Set([
  "InvalidClientTokenId",
  "SignatureDoesNotMatch",
  "AuthFailure",
  "MissingAuthenticationToken",
  "IncompleteSignature",
  "UnrecognizedClientException",
  "ExpiredToken",
  "ExpiredTokenException",
  // S3/predastore reports a stale key as InvalidAccessKeyId rather than the
  // gateway's InvalidClientTokenId.
  "InvalidAccessKeyId",
])

// The boundary for a caught value: anything can be thrown, so this predicate is
// what gives one a shape.
// oxlint-disable-next-line anti-slop/no-unknown-parameters -- classifies arbitrary caught values
function hasErrorCode(err: unknown): err is { name: string } {
  return (
    typeof err === "object" &&
    err !== null &&
    "name" in err &&
    typeof err.name === "string"
  )
}

// isStaleCredentialsError reports whether an error came from stale or invalid
// credentials, keying off the AWS error code (err.name) rather than the message
// or bare HTTP status. Both SDK errors and signed-fetch errors expose the code
// on `name`.
// oxlint-disable-next-line anti-slop/no-unknown-parameters -- called from catch blocks and cache handlers
export function isStaleCredentialsError(err: unknown): boolean {
  return hasErrorCode(err) && AUTH_ERROR_CODES.has(err.name)
}
