### Trust policies

Trust policies are validated when they are written.
`NotPrincipal`, `NotAction`, empty `Action` elements and empty `Principal` blocks are rejected as malformed.

`Condition` blocks are rejected, except `StringEquals` on `AssumeRoleWithWebIdentity`, which IRSA uses.
Spinifex evaluates that condition against the token's issuer, subject and audience.

### Parameters that are refused

Session policies, policy ARNs, session tags, and MFA serial numbers and token codes are rejected.
A session always has the full permissions of its role.
