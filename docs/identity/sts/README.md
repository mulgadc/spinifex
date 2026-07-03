---
title: "STS and Temporary Credentials"
description: "Mint short-lived credentials with STS — assume roles, issue session tokens, and federate Kubernetes workloads with OIDC."
category: "Identity"
tags:
  - sts
  - iam
  - temporary credentials
  - assume role
  - oidc
  - security
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "AWS STS Documentation"
    url: "https://docs.aws.amazon.com/STS/latest/APIReference/welcome.html"
  - title: "AWS Temporary Security Credentials"
    url: "https://docs.aws.amazon.com/IAM/latest/UserGuide/id_credentials_temp.html"
---

# STS and Temporary Credentials

> Mint short-lived credentials with STS — assume roles, issue session tokens, and federate Kubernetes workloads with OIDC.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Checking Your Identity](#checking-your-identity)
- [Assuming a Role](#assuming-a-role)
- [Using Temporary Credentials](#using-temporary-credentials)
- [Session Tokens for Your Own User](#session-tokens-for-your-own-user)
- [Web Identity Federation (IRSA)](#web-identity-federation-irsa)
- [Trust Policy Validation](#trust-policy-validation)
- [Troubleshooting](#troubleshooting)

---

## Overview

STS (Security Token Service) issues **temporary credentials**: an `ASIA`-prefixed access key, a secret key, and a session token, all of which expire together at a fixed time. Nothing to rotate, nothing to revoke — the credentials simply stop working.

Spinifex supports the three main ways to obtain them:

- **`assume-role`** — exchange your IAM user credentials for a [role's](/docs/iam-roles-and-instance-profiles) permissions, gated by the role's trust policy.
- **`get-session-token`** — get a time-boxed copy of your own user's permissions.
- **`assume-role-with-web-identity`** — exchange an OIDC ID token (a Kubernetes ServiceAccount token) for role credentials, with no IAM credentials at all.

EC2 instances get role credentials a fourth way, automatically through [IMDS](/docs/imds) — no STS call needed in the guest.

## Prerequisites

- A running Spinifex cluster with the AWS gateway started
- AWS CLI configured with your account's profile. The default `spinifex` account created at cluster setup works out of the box; to work in a separate account, create one with `spx admin account create` (see [Spinifex Admin CLI](/docs/spinifex-admin-cli)):

```bash
export AWS_PROFILE=spinifex
```

## Instructions

## Checking Your Identity

`get-caller-identity` returns whoever signed the request — it needs no permissions and never fails for authorisation reasons, which makes it the standard "which credentials am I using?" check:

```bash
aws sts get-caller-identity
```

```json
{
    "UserId": "AIDA1A2B3C4D5E6F7890",
    "Account": "000000000001",
    "Arn": "arn:aws:iam::000000000001:user/admin"
}
```

Run with temporary role credentials, the ARN switches to the assumed-role form — see [Using Temporary Credentials](#using-temporary-credentials).

## Assuming a Role

`assume-role` works when the target role's trust policy allows your principal. Create a role that trusts an IAM user (see [IAM Roles and Instance Profiles](/docs/iam-roles-and-instance-profiles) for the full role lifecycle):

```bash
cat > /tmp/deploy-trust.json << 'EOF'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "AWS": "arn:aws:iam::000000000001:user/admin" },
      "Action": "sts:AssumeRole"
    }
  ]
}
EOF

aws iam create-role --role-name deploy \
  --assume-role-policy-document file:///tmp/deploy-trust.json

aws iam put-role-policy --role-name deploy --policy-name ec2-read \
  --policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["ec2:Describe*"],"Resource":"*"}]}'
```

Then assume it:

```bash
aws sts assume-role \
  --role-arn arn:aws:iam::000000000001:role/deploy \
  --role-session-name release-42
```

```json
{
    "Credentials": {
        "AccessKeyId": "ASIA1A2B3C4D5E6F7890",
        "SecretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
        "SessionToken": "IQoJb3JpZ2luX2VjE...",
        "Expiration": "2026-07-03T11:00:00Z"
    },
    "AssumedRoleUser": {
        "AssumedRoleId": "AROA1A2B3C4D5E6F7890:release-42",
        "Arn": "arn:aws:sts::000000000001:assumed-role/deploy/release-42"
    },
    "PackedPolicySize": 0
}
```

The session name tags every session so audit logs can tell *who* used the role; it appears in the assumed-role ARN and the `UserId`.

If the trust policy does not allow your principal, `assume-role` fails with `AccessDenied` — no matter what IAM permissions you hold.

### Session Duration

`--duration-seconds` must be between **900** (15 minutes) and the smaller of the role's `MaxSessionDuration` and **43200** (12 hours); the default is **3600** (1 hour). A value outside that window is rejected with `ValidationError`, so to run longer sessions, raise the role's ceiling first:

```bash
# 7200 > MaxSessionDuration (3600) → ValidationError
aws sts assume-role --role-arn arn:aws:iam::000000000001:role/deploy \
  --role-session-name long-build --duration-seconds 7200

aws iam update-role --role-name deploy --max-session-duration 43200

aws sts assume-role --role-arn arn:aws:iam::000000000001:role/deploy \
  --role-session-name long-build --duration-seconds 7200
```

### Flags That Differ from AWS

- `--external-id` and `--source-identity` are accepted and logged but **not enforced** — trust policies cannot carry the `Condition` blocks that would check them (see [Trust Policy Validation](#trust-policy-validation)). Do not rely on an external ID as a security boundary.
- `--serial-number` / `--token-code` (MFA) are not supported and return `InvalidParameterValue`.
- `--policy` / `--policy-arns` (session policies) are not supported and return `PackedPolicyTooLarge`.

## Using Temporary Credentials

Temporary credentials are used exactly like access keys, plus a third value — the session token. Export all three as environment variables (which take precedence over any profile):

```bash
export AWS_ACCESS_KEY_ID=ASIA1A2B3C4D5E6F7890
export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
export AWS_SESSION_TOKEN=IQoJb3JpZ2luX2VjE...
export AWS_ENDPOINT_URL=https://localhost:9999
export AWS_CA_BUNDLE=/var/lib/spinifex/config/ca.pem
export AWS_DEFAULT_REGION=ap-southeast-2
```

Or store them in a named profile — the same shape as a user profile with `aws_session_token` added:

```bash
aws configure set aws_access_key_id ASIA1A2B3C4D5E6F7890 --profile spinifex-deploy
aws configure set aws_secret_access_key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY --profile spinifex-deploy
aws configure set aws_session_token IQoJb3JpZ2luX2VjE... --profile spinifex-deploy
aws configure set region ap-southeast-2 --profile spinifex-deploy
aws configure set endpoint_url https://localhost:9999 --profile spinifex-deploy
aws configure set ca_bundle /var/lib/spinifex/config/ca.pem --profile spinifex-deploy
```

Requests are then authorised as the assumed role, not your user:

```bash
aws sts get-caller-identity
# "Arn": "arn:aws:sts::000000000001:assumed-role/deploy/release-42"

aws ec2 describe-instances     # allowed by the role's ec2-read policy
aws iam list-users             # AccessDenied — the role grants ec2:Describe* only
```

When the expiration time passes, every call fails until you fetch fresh credentials — there is no refresh; run `assume-role` again.

## Session Tokens for Your Own User

`get-session-token` issues temporary credentials with your **own user's** permissions — no role, no trust policy. Useful for handing a build script credentials that expire on their own instead of your long-lived access key:

```bash
aws sts get-session-token --duration-seconds 3600
```

```json
{
    "Credentials": {
        "AccessKeyId": "ASIA1A2B3C4D5E6F7890",
        "SecretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
        "SessionToken": "IQoJb3JpZ2luX2VjE...",
        "Expiration": "2026-07-03T11:00:00Z"
    }
}
```

Duration runs from **900** to **129600** seconds (36 hours), defaulting to **43200** (12 hours). Unlike `assume-role`, out-of-range values are **clamped, not rejected** — asking for 200000 seconds quietly returns a 36-hour session. Use the credentials exactly as in [Using Temporary Credentials](#using-temporary-credentials); `get-caller-identity` keeps reporting your user ARN, since no role is involved.

MFA (`--serial-number` / `--token-code`) is not supported and returns `InvalidParameterValue`.

## Web Identity Federation (IRSA)

`assume-role-with-web-identity` exchanges an OIDC ID token for role credentials — the mechanism behind **IRSA** (IAM Roles for Service Accounts), where a Kubernetes pod's ServiceAccount token becomes its cloud identity. The call is anonymous: the JWT is the identity, so no IAM credentials sign the request.

In Spinifex the token issuer is an [EKS](/docs/eks) cluster. Each cluster publishes a signing-key set (JWKS) under an issuer URL of the form `https://{host}/oidc/eks/{region}/{account-id}/{cluster-name}`, and STS verifies tokens against it directly — no external identity provider is contacted.

### Register the OIDC Provider

The role account must register the issuer as an IAM OIDC provider before any token from it is accepted:

```bash
aws iam create-open-id-connect-provider \
  --url https://spinifex.example.com/oidc/eks/ap-southeast-2/000000000001/prod \
  --client-id-list sts.amazonaws.com \
  --thumbprint-list 9e99a48a9960b14926bb7f3b02e22da2b0ab7280
```

```json
{
    "OpenIDConnectProviderArn": "arn:aws:iam::000000000001:oidc-provider/spinifex.example.com/oidc/eks/ap-southeast-2/000000000001/prod",
    "Tags": []
}
```

Manage providers with the usual verbs — `get-open-id-connect-provider` and `list-open-id-connect-provider-tags` take the ARN, and tags work like every other IAM resource:

```bash
aws iam list-open-id-connect-providers
aws iam get-open-id-connect-provider \
  --open-id-connect-provider-arn arn:aws:iam::000000000001:oidc-provider/spinifex.example.com/oidc/eks/ap-southeast-2/000000000001/prod
aws iam delete-open-id-connect-provider \
  --open-id-connect-provider-arn arn:aws:iam::000000000001:oidc-provider/spinifex.example.com/oidc/eks/ap-southeast-2/000000000001/prod
```

### The IRSA Trust Policy

The role's trust policy names the provider as a `Federated` principal and pins the token's `sub` (the ServiceAccount) and `aud` claims with `StringEquals` — the one place a `Condition` block is allowed:

```bash
cat > /tmp/irsa-trust.json << 'EOF'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::000000000001:oidc-provider/spinifex.example.com/oidc/eks/ap-southeast-2/000000000001/prod"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "spinifex.example.com/oidc/eks/ap-southeast-2/000000000001/prod:sub": "system:serviceaccount:default:app",
          "spinifex.example.com/oidc/eks/ap-southeast-2/000000000001/prod:aud": "sts.amazonaws.com"
        }
      }
    }
  ]
}
EOF

aws iam create-role --role-name irsa-app \
  --assume-role-policy-document file:///tmp/irsa-trust.json
```

The condition keys are the issuer URL (scheme stripped) followed by `:sub` or `:aud`. Only these two keys and only `StringEquals` are accepted — anything wider (`StringLike`, wildcards, other keys) is rejected at `create-role` with `MalformedPolicyDocument` rather than silently over-granting.

### Exchange the Token

```bash
aws sts assume-role-with-web-identity \
  --role-arn arn:aws:iam::000000000001:role/irsa-app \
  --role-session-name pod-app-7f9c4 \
  --web-identity-token "$(cat /var/run/secrets/eks.amazonaws.com/serviceaccount/token)"
```

The response has the same `Credentials` block as `assume-role`, plus the verified `SubjectFromWebIdentityToken`, `Provider`, and `Audience`. Duration runs 900–43200 seconds (default 3600), independent of the role's `MaxSessionDuration`.

For a token to be accepted it must be an ES256-signed JWT whose `iss` matches a registered OIDC provider in the role's account, whose `aud` contains `sts.amazonaws.com`, whose signature verifies against the cluster's published JWKS, and which has not expired — any failure returns `InvalidIdentityToken`. In-cluster, none of this is manual: pods with an annotated ServiceAccount get the token mounted and the AWS SDK performs the exchange automatically.

## Trust Policy Validation

Spinifex validates trust policies (`AssumeRolePolicyDocument`) more strictly than AWS, rejecting at write time with `MalformedPolicyDocument`:

- `Condition` blocks anywhere **except** the `StringEquals` IRSA form on `sts:AssumeRoleWithWebIdentity` described above. This is why `--external-id` cannot be enforced.
- `NotPrincipal` and `NotAction` elements.
- Empty `Principal` blocks and empty-string `Action` elements.

Everything the validator accepts is evaluated at assume time; explicit `Deny` statements win, and a policy that matches no statement denies.

## Troubleshooting

### AccessDenied When Assuming a Role

The role's trust policy does not allow your principal, or the role does not exist — a missing role is deliberately indistinguishable from a denied one, matching AWS. Check who you are, that the role exists, and what it trusts:

```bash
aws sts get-caller-identity
aws iam list-roles --query 'Roles[].Arn'
aws iam get-role --role-name deploy \
  --query 'Role.AssumeRolePolicyDocument'
```

For web identity, `AccessDenied` also covers a trust-policy `Condition` that doesn't match the token's `sub`/`aud` claims.

### ValidationError on Duration

The requested `--duration-seconds` is outside 900–min(`MaxSessionDuration`, 43200). Check and raise the role's ceiling:

```bash
aws iam get-role --role-name deploy --query 'Role.MaxSessionDuration'
aws iam update-role --role-name deploy --max-session-duration 43200
```

Note `get-session-token` never returns this — it clamps instead.

### InvalidParameterValue

You passed an MFA flag (`--serial-number` / `--token-code`) — MFA is not supported. `--tags` and `--transitive-tag-keys` on `assume-role` return the same error.

### PackedPolicyTooLarge

You passed `--policy` or `--policy-arns`. Session policies are not supported — scope the role's own permissions instead.

### InvalidIdentityToken

The web identity token failed verification: malformed JWT, wrong signature algorithm (only ES256 is accepted), expired, `aud` missing `sts.amazonaws.com`, issuer not registered as an OIDC provider in the role's account, or the cluster's JWKS is unavailable. Confirm the provider is registered:

```bash
aws iam list-open-id-connect-providers
```

### Credentials Suddenly Rejected

Temporary credentials expired. Check the `Expiration` from the original response and mint a fresh set — sessions cannot be renewed or extended.
