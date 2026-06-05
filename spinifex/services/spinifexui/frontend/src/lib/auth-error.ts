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
])

// isStaleCredentialsError reports whether an error came from stale or invalid
// credentials, keying off the AWS error code (err.name) rather than the message
// or bare HTTP status. Both SDK errors and signed-fetch errors expose the code
// on `name`.
export function isStaleCredentialsError(err: unknown): boolean {
  if (typeof err !== "object" || err === null || !("name" in err)) {
    return false
  }
  const { name } = err
  return typeof name === "string" && AUTH_ERROR_CODES.has(name)
}
