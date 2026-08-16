---
title: "Account Management"
description: "Create, list and delete Spinifex tenant accounts from the CLI or over the private admin API."
category: "Admin"
tags:
  - accounts
  - iam
  - admin api
  - multi-tenancy
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "Spinifex Admin CLI"
    url: "/docs/spinifex-admin-cli"
---

# Account Management

> Create, list and delete tenant accounts — from an operator shell, or from your own signup and provisioning systems.

## Table of Contents

- [Overview](#overview)
- [1. The accounts a cluster starts with](#1-the-accounts-a-cluster-starts-with)
- [2. Create an admin principal](#2-create-an-admin-principal)
- [3. Create an account](#3-create-an-account)
- [4. List accounts](#4-list-accounts)
- [5. Delete an account](#5-delete-an-account)
- [6. Calling the admin API from your own service](#6-calling-the-admin-api-from-your-own-service)
- [7. Operating the credential](#7-operating-the-credential)
- [Troubleshooting](#troubleshooting)

---

## Overview

A Spinifex cluster is multi-tenant. Each tenant is an **account**: a 12-digit ID that owns its own VPCs, instances, volumes, buckets, IAM users and keys. Resources in one account are invisible to every other account, including to the super-admin account.

There are two ways to manage accounts, and they do the same work:

| | Where it runs | What it needs |
|---|---|---|
| **`spx admin account …`** | On a cluster node | A shell on the node, and the cluster's own NATS credentials |
| **`POST /admin/<Method>`** | Anywhere that can reach the gateway | A SigV4 credential from an **admin principal** |

The API exists so a self-service signup page, a provisioning system, a billing job or a load-test harness can create and remove tenants without an operator shell on a node. It is a Spinifex-internal surface, not an AWS API — the AWS CLI cannot call it.

The same `spx` commands drive both: add `--remote` and they sign an HTTPS request to the gateway instead of connecting to NATS. That makes `spx` the reference client for the API as well as the operator tool.

## 1. The accounts a cluster starts with

`spx admin init` seeds two accounts before any tenant exists:

| Account ID | Purpose |
|---|---|
| `000000000000` | The system account. Internal platform resources — system AMIs and the cluster's own objects — live here. |
| `000000000001` | The **super-admin** account. Its `admin` user is the credential printed at the end of `spx admin init`. |

Both are refused by `DeleteAccount`. Tenants are numbered from `000000000002` upward.

The private admin API is served **only** for principals in `000000000001`. A key from a tenant account is denied even if that tenant's own policy grants the action, and so is an assumed-role session from the super-admin account itself. That restriction is deliberate: creating and deleting tenants is a platform operation, not something a tenant can be delegated.

Because of it, `spx admin principal create` always works in `000000000001`. There is no `--account-id`: granting these actions to a principal anywhere else would produce a credential the gateway refuses.

### What the bootstrap admin can already do

The `admin` user in `000000000001` is created with the `AdministratorAccess` policy, whose action is `*`. The gateway evaluates the caller's policy for `spinifex:<Method>`, and `*` matches — so **the bootstrap credential can already call every admin method**, with no extra setup.

That is fine for a first look from a shell. It is a poor credential to give a service:

- it is unscoped — the same key that reads a listing can delete every tenant;
- it is the account's administrator, so rotating it to contain a leak locks the operator out of everything else.

For anything long-lived, mint a named principal instead.

## 2. Create an admin principal

An **admin principal** is an IAM user in the super-admin account with an inline policy naming the admin methods it may call. Create one per consumer.

```bash
spx admin principal create operator
```

```
Principal "operator" ready.
  Account ID:        000000000001
  User:              operator
  Permitted actions: spinifex:CreateAccount, spinifex:DeleteAccount, spinifex:DescribeAccountDeletion, spinifex:ListAccounts
  Access Key ID:     AKIA...
  Secret Access Key: ...

The secret is shown once and is not recoverable. Store it in a secret manager
or an AWS profile; never commit it and never place it in a Spinifex config file.

Requests must be SigV4-signed with service="spinifex" and this cluster's region.
```

Without `--grant` the principal may call every admin method. Scope it with `--grant` when the consumer needs less:

```bash
# A public signup form: it creates accounts and can do nothing else.
spx admin principal create signup --grant CreateAccount

# A billing or inventory job: read-only.
spx admin principal create billing --grant ListAccounts,DescribeAccountDeletion
```

Scoping matters most for anything internet-facing. A signup page's credential lives wherever that page runs; holding only `CreateAccount`, a leak of it costs capped junk accounts, not every tenant on the cluster.

Each method is granted by name rather than as `spinifex:*`, so a key minted today is not silently authorised for a method added to the surface tomorrow.

A principal's grants are exactly one inline policy. Re-running `create` with fewer `--grant` values narrows the principal, and any other inline policy on the user is removed — reducing a credential's reach never leaves the old grant behind.

Store the secret as an AWS profile if you will use `spx --remote` or the AWS SDKs:

```ini
# ~/.aws/credentials
[spinifex-operator]
aws_access_key_id = AKIA...
aws_secret_access_key = ...
```

Check what exists at any time:

```bash
spx admin principal list
```

```
Principals in the super-admin account (000000000001):

USER                     KEYS   GRANTS
------------------------------------------------------------------------------------------------
admin                    1      AdministratorAccess (attached)
operator                 1      CreateAccount, DeleteAccount, DescribeAccountDeletion, ListAccounts
signup                   1      CreateAccount

3 principal(s)
```

An attached policy is reported by name rather than expanded — `AdministratorAccess (attached)` is how an unscoped principal shows up in the listing.

## 3. Create an account

On a node:

```bash
spx admin account create --name customer@example.com
```

Over the API, from anywhere:

```bash
AWS_PROFILE=spinifex-operator spx admin account create --remote \
  --name customer@example.com \
  --endpoint https://node1.example.com:9999 \
  --region us-west-1 \
  --ca-bundle ~/.spinifex/ca.pem
```

Either way the cluster creates, in one call:

- the account record and its 12-digit ID;
- an `admin` IAM user in it, with `AdministratorAccess` and one access key;
- a default VPC.

```
Account created successfully!
  Account ID:        000000000042
  Account Name:      customer@example.com
  Admin User:        admin
  Access Key ID:     AKIA...
  Secret Access Key: ...
  Default VPC:       vpc-...
  Client Token:      3f9c...
```

**The secret is returned once.** Hand it to the customer, or store it, at the moment you receive it.

### Client tokens

`CreateAccount` is idempotent on `clientToken`. Replaying the same token within 24 hours returns the identical response — including the secret — rather than creating a second account. That is the only way to re-obtain a secret you lost to a dropped connection.

The rule that follows: **retry with the same token, never a fresh one.** A fresh token after a timeout is how a duplicate account gets made. `spx` prints the token it used for exactly this reason, and only suggests a retry for the error codes that are safe to retry (`OperationInProgress`, `ServiceUnavailable`, `InternalError`).

Account names are unique cluster-wide, case- and whitespace-insensitive. `[signup] max_accounts` in `awsgw.toml` caps how many accounts may exist — 128 by default, 0 for uncapped.

## 4. List accounts

```bash
spx admin account list                      # on a node
AWS_PROFILE=spinifex-operator spx admin account list --remote \
  --endpoint https://node1.example.com:9999 --region us-west-1
```

```
ACCOUNT ID     NAME                      STATUS       CREATED
000000000001   admin                     ACTIVE       2026-06-01 09:14
000000000042   customer@example.com      ACTIVE       2026-08-16 07:00
```

`STATUS` is worth watching. An account left in `TERMINATING` is a teardown that did not finish — see [Troubleshooting](#troubleshooting).

## 5. Delete an account

Deleting an account removes everything it owns. Look before you leap:

```bash
spx admin account delete 000000000042 --dry-run
```

`--dry-run` lists the inventory and changes nothing. It needs neither the account name nor a client token.

Then delete for real:

```bash
spx admin account delete 000000000042
```

The command prints the inventory, asks for the account **name** as confirmation (type it, or pass `--name` with `--yes` for automation), then works through seven stages in dependency order:

| Stage | What it removes |
|---|---|
| compute | Instances, and anything running on them |
| attachments | Volume attachments, ENI attachments, address associations |
| storage | Volumes, snapshots, buckets and their objects |
| network | Subnets, route tables, gateways, security groups, VPCs |
| platform | Higher-level services built on the above |
| identity | IAM users, keys, policies and roles |
| account | The name reservation, the quota counter and the account record |

Each stage runs to empty before the next begins, so nothing is deleted while something still depends on it. The account is marked `TERMINATING` first, which blocks its credentials cluster-wide — a tenant cannot create new resources into a teardown that is already walking past them.

### It returns before it finishes

A large account takes minutes to tear down. The API does not hold the connection open for it: `DeleteAccount` starts the job, returns a `deletionId` immediately, and progress is read from `DescribeAccountDeletion`.

```bash
AWS_PROFILE=spinifex-operator spx admin account delete 000000000042 --remote \
  --name customer@example.com --yes \
  --endpoint https://node1.example.com:9999 --region us-west-1
```

`spx --remote` starts the job and then polls, printing each stage as it lands. Your own client can do the same, or start the job and check back later — the deletion record outlives the account, so `DescribeAccountDeletion` stays answerable after the account is gone.

One job runs per account. A second `DeleteAccount` with a different token while one is running returns `OperationInProgress`; with the *same* token it returns the running job, which is what makes a retry safe.

### When something will not delete

Anything that refuses to go is reported as **stuck**, with the reason, and the account stays `TERMINATING` so the residue keeps an owner. Re-running the delete resumes from where it stopped.

For the case where two resources hold each other — a volume attached to an instance that will not terminate, so neither can be deleted — use `--force`:

```bash
spx admin account delete 000000000042 --force --name customer@example.com --yes
```

`--force` escalates only where the ordinary API refuses: it clears an attachment in the control plane without the guest's cooperation, hard-destroys after a graceful attempt times out, and treats an already-missing resource as deleted. It never reorders the stages and never touches anything outside the account.

## 6. Calling the admin API from your own service

The surface is `POST https://<gateway>:9999/admin/<Method>`, JSON in and JSON out. Requests are SigV4-signed with:

- **service** — `spinifex`. Not `ec2`, not `iam`. A key signed for another service is treated as probing and denied.
- **region** — the cluster's configured region (`region` in `awsgw.toml`).
- **credentials** — an admin principal's access key.

| Method | Request | Response |
|---|---|---|
| `CreateAccount` | `name`, `clientToken` (32–128 chars of `[A-Za-z0-9_-]`), `source` | `accountId`, `accountName`, `adminUser`, `accessKeyId`, `secretAccessKey`, `defaultVpcId` |
| `DeleteAccount` | `accountId`, `accountName`, `clientToken`, `force`, `dryRun` | `deletionId`, `accountId`, `state`; plus `inventory` when `dryRun` |
| `DescribeAccountDeletion` | `accountId` | `deletionId`, `state`, `startedAt`, `updatedAt`, `finishedAt`, `stages[]`, `error` |
| `ListAccounts` | — | `accounts[]`, `count` |

Errors are `{"error":{"code":…,"message":…},"requestId":…}`. Every response carries `X-Amzn-RequestId`, repeated as `requestId` in an error body — quote it when reporting a problem.

Retry `OperationInProgress` (409), `ServiceUnavailable` (503) and `InternalError` (500) with backoff, **always reusing the same `clientToken`**. Do not retry `AccountAlreadyExists`, `IdempotentParameterMismatch`, `LimitExceeded`, `InvalidParameterValue`, `MissingParameter`, `InvalidRequest`, `InvalidAction`, `MethodNotAllowed` or `AccessDenied`.

Any SigV4 library will sign these. With `botocore` in Python:

```python
import json, requests
from botocore.auth import SigV4Auth
from botocore.awsrequest import AWSRequest
from botocore.session import Session

ENDPOINT = "https://node1.example.com:9999"
REGION = "us-west-1"

def call_admin(method, body, profile="spinifex-operator"):
    url = f"{ENDPOINT}/admin/{method}"
    payload = json.dumps(body)
    request = AWSRequest(method="POST", url=url, data=payload,
                         headers={"Content-Type": "application/json"})
    credentials = Session(profile=profile).get_credentials()
    # The credential scope is "spinifex" — the gateway denies any other service.
    SigV4Auth(credentials, "spinifex", REGION).add_auth(request)

    response = requests.post(url, data=payload, headers=dict(request.headers),
                             verify="/path/to/cluster-ca.pem", timeout=90)
    response.raise_for_status()
    return response.json()

account = call_admin("CreateAccount", {
    "name": "customer@example.com",
    "clientToken": "signup-9f2c1e7b4a0d8356",   # store this before you send it
    "source": "signup-web",
})
```

To confirm the route is served after a deployment, an unauthenticated request is enough:

```bash
curl -sk -X POST https://node1.example.com:9999/admin/ListAccounts
```

`403 MissingAuthenticationToken` means the surface is up. `404` means it is not.

## 7. Operating the credential

**Rotate** by re-running create. It replaces the access key, which revokes the previous one:

```bash
spx admin principal create operator
```

**Revoke** without removing the principal — the response to a leaked secret:

```bash
spx admin principal revoke signup
```

**Remove** a principal that should not exist at all — its keys, its policies and the user:

```bash
spx admin principal delete probe
```

Revocation and deletion take effect immediately and cluster-wide. No restart, no config change.

**Audit** what an admin principal could escalate into:

```bash
spx admin principal audit
```

STS does not evaluate the caller's identity policy on `AssumeRole`. A role in the super-admin account whose trust policy names the account, its root ARN, or a wildcard is therefore assumable by *any* principal in that account — including one scoped to `CreateAccount` — which then inherits the role's permissions. `audit` lists those roles and exits non-zero if it finds any. Scope their trust policies to a specific principal.

Rules worth keeping:

- One principal per consumer. Shared credentials cannot be revoked independently.
- Grant the least the consumer needs. A signup form needs `CreateAccount`.
- Never put an admin secret in a Spinifex config file, an AMI, or a repository.
- The `admin` user of `000000000001` is a break-glass credential, not a service credential.

## Troubleshooting

### `AccessDenied` on every admin call

The surface returns the same `AccessDenied` for every failed gate, so it cannot be used to work out which one you failed. Check each:

- Is the request signed with **`service=spinifex`**? A key signed for `ec2` or `iam` is denied here.
- Is the region the cluster's configured region?
- Does the credential belong to a user in **`000000000001`**? A tenant account's key is denied. Confirm with `spx admin principal list` on a node.
- Is it a **user's** long-lived key, not an assumed-role session?
- Does the principal hold `spinifex:<Method>` for the method you called? `spx admin principal list` shows the grants.

The gateway's log names the failed gate, so `journalctl -u spinifex-awsgw` on the node that served the request is the fastest answer.

### A principal's key stopped working

`spx admin principal create <name>` revokes the previous key by design — someone re-running it to mint a key for another machine invalidates yours. Re-run it and distribute the new secret, or create a second principal so each holder has its own.

### An account is stuck in `TERMINATING`

The teardown could not remove something. Ask what it stopped on:

```bash
spx admin account delete 000000000042 --dry-run
```

The inventory lists what remains. Re-run the delete to resume; if two resources hold each other, add `--force`.

A teardown whose gateway restarted mid-job stalls with a `RUNNING` record. After five minutes without progress the job is treated as abandoned, and a fresh `DeleteAccount` takes it over.

### `IdempotentParameterMismatch`

The client token has been used before with different parameters — usually a token reused across two different account names. Tokens are per-request, not per-caller. Generate one at the point you build the request and store it with the pending record.

### A duplicate account appeared

A create that timed out was retried with a **new** client token. The first call had succeeded. Delete the extra account, and change the caller to persist its token before sending and reuse it on every retry.
