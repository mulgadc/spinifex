---
title: "IMDS (Instance Metadata Service)"
description: "Access instance metadata, user data, and IAM role credentials from inside a guest VM with IMDSv2."
category: "Identity"
tags:
  - imds
  - metadata
  - instance identity
  - iam roles
  - credentials
  - security
resources:
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "AWS IMDS Documentation"
    url: "https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-metadata.html"
  - title: "AWS IMDSv2 Deep Dive"
    url: "https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/configuring-instance-metadata-service.html"
---

# Instance Metadata Service (IMDS)

> Access instance metadata, user data, and IAM role credentials from inside a guest VM with IMDSv2.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Using IMDS from a Guest](#using-imds-from-a-guest)
- [Metadata Paths](#metadata-paths)
- [User Data](#user-data)
- [IAM Role Credentials](#iam-role-credentials)
- [Instance Identity Document](#instance-identity-document)
- [Metadata Options](#metadata-options)
- [How It Works](#how-it-works)
- [Limits and Defaults](#limits-and-defaults)
- [Command Reference](#command-reference)
- [Troubleshooting](#troubleshooting)

---

## Overview

Every running guest VM can reach the Instance Metadata Service at `http://169.254.169.254`, exactly as on EC2. There is no in-VM agent to install and no in-guest route configuration — DHCP and fully static guests reach it identically.

IMDS answers questions a workload asks about itself: instance ID, instance type, private and public IPs, hostname, availability zone, security groups, the launch SSH key, user data, and — when the instance has an IAM instance profile — short-lived, auto-rotating role credentials.

**Spinifex is IMDSv2-only.** Every read requires a session token obtained with a `PUT` request. A tokenless (IMDSv1-style) `GET` returns `401 Unauthorized` with an empty body.

Requests are attributed to an instance by the network interface they arrive on and tokens are bound to that interface, so one instance can never read another's metadata or replay its tokens.

## Prerequisites

- A running Spinifex cluster (see [Launching Instances](/docs/launching-instances))
- A running instance to query from
- For IAM role credentials: an instance launched with `--iam-instance-profile`

## Instructions

## Using IMDS from a Guest

All commands in this section run **inside the guest VM**.

### Get a Session Token

Request a token with a TTL between 1 and 21600 seconds (6 hours):

```bash
TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 21600")
```

The token endpoint only accepts `PUT` — a `GET` returns `405 Method Not Allowed`. A missing or out-of-range TTL header returns `400 Bad Request`.

### Read Metadata

Send the token back in the `X-aws-ec2-metadata-token` header on every read:

```bash
curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/instance-id
```

```
i-0a1b2c3d4e5f67890
```

Directory paths (ending in `/`) return a newline-separated listing of children:

```bash
curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/
```

### One-Liner Pattern

For scripts, mint a short-lived token inline:

```bash
imds() {
  local token=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
  curl -s -H "X-aws-ec2-metadata-token: $token" "http://169.254.169.254/latest/$1"
}

imds meta-data/instance-id
imds meta-data/local-ipv4
imds meta-data/placement/availability-zone
```

## Metadata Paths

`GET /` lists supported API versions (`2021-07-15`, `latest`). Any dated version segment (e.g. `/2016-09-02/...`, as probed by cloud-init) aliases to `/latest`.

Commonly used paths under `/latest/meta-data/`:

| Path | Returns |
| ---- | ------- |
| `instance-id` | Instance ID |
| `instance-type` | Instance type (e.g. `t3.micro`) |
| `ami-id` | Image the instance was launched from |
| `ami-launch-index` | Launch index within the reservation (`0..n-1`) |
| `reservation-id` | Reservation ID |
| `instance-life-cycle` | `spot` or `on-demand` |
| `local-ipv4` | Primary private IP |
| `public-ipv4` | Elastic/public IP; 404 when none |
| `public-hostname` | Mirrors `public-ipv4`; 404 when no public IP |
| `mac` | Primary interface MAC address |
| `hostname`, `local-hostname` | `ip-<dashed-ip>.<region>.compute.internal` |
| `security-groups` | Security group names, one per line |
| `placement/availability-zone` | Availability zone |
| `placement/region` | Region (AZ with trailing letter stripped) |
| `services/domain`, `services/partition` | `amazonaws.com` / `aws` |
| `public-keys/0/openssh-key` | Launch key pair's SSH public key; 404 if no key pair or the key was deleted |
| `iam/info` | Instance profile ARN and ID; 404 if no profile |
| `iam/security-credentials/<role>` | Temporary role credentials (see below) |
| `network/interfaces/macs/<mac>/...` | Primary interface subtree: `interface-id`, `owner-id`, `subnet-id`, `vpc-id`, `local-ipv4s`, `security-group-ids`, `subnet-ipv4-cidr-block`, `vpc-ipv4-cidr-block`, and more |

The `network/interfaces/macs/` subtree covers the **primary interface only**; querying another MAC returns 404 (multi-ENI metadata is deferred).

Paths that intentionally return **404**: `tags/instance/*`, `block-device-mapping/*`, `placement/{group-name,partition-number,availability-zone-id,host-id}`, `instance-action`, and `spot/{instance-action,termination-time}` (404 is the correct "no interruption scheduled" answer for spot pollers).

## User Data

User data supplied at launch (`run-instances --user-data`) is served at:

```bash
curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/user-data
```

Returns the decoded user data, or 404 if the instance was launched without any. cloud-init consumes this automatically at first boot.

## IAM Role Credentials

Instances launched with an IAM instance profile get short-lived, auto-rotating credentials through IMDS — no static keys baked into the image. Creating roles and instance profiles is covered in [IAM Roles and Instance Profiles](/docs/iam-roles-and-instance-profiles).

```bash
# Discover the role name
ROLE=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/iam/security-credentials/)

# Fetch credentials
curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  "http://169.254.169.254/latest/meta-data/iam/security-credentials/$ROLE"
```

```json
{
  "Code": "Success",
  "LastUpdated": "2026-07-03T10:00:00Z",
  "Type": "AWS-HMAC",
  "AccessKeyId": "ASIA1A2B3C4D5E6F7890",
  "SecretAccessKey": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
  "Token": "IQoJb3JpZ2luX2VjE...",
  "Expiration": "2026-07-03T11:00:00Z",
  "AccountId": "000000000001"
}
```

Credentials are ASIA-prefixed temporary STS credentials valid for **1 hour**, re-minted automatically **5 minutes before expiry**. The AWS SDKs and CLI pick them up with no configuration:

```bash
# Inside the guest, with no ~/.aws/credentials:
aws sts get-caller-identity
```

If the instance has no profile, the whole `iam/` subtree returns 404 (and is omitted from the `meta-data/` listing).

## Instance Identity Document

An unsigned identity document (schema `2017-09-30`) is available at:

```bash
curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/dynamic/instance-identity/document
```

```json
{
  "accountId": "000000000001",
  "architecture": "x86_64",
  "availabilityZone": "ap-southeast-2a",
  "imageId": "ami-0123456789abcdef0",
  "instanceId": "i-0a1b2c3d4e5f67890",
  "instanceType": "t3.micro",
  "pendingTime": "2026-07-03T09:58:12Z",
  "privateIp": "10.0.1.15",
  "region": "ap-southeast-2",
  "version": "2017-09-30"
}
```

The signed forms (`signature`, `pkcs7`, `rsa2048`) currently return 404 — they require a per-cluster signing key and land with EKS IRSA support.

## Metadata Options

Metadata options are managed from the **control plane** with the standard EC2 commands. Because the platform is IMDSv2-only, the only mutable knob is the PUT response hop limit.

### Inspect

```bash
aws ec2 describe-instances --instance-ids i-0a1b2c3d4e5f67890 \
  --query 'Reservations[0].Instances[0].MetadataOptions'
```

```json
{
  "State": "applied",
  "HttpTokens": "required",
  "HttpPutResponseHopLimit": 1,
  "HttpEndpoint": "enabled",
  "HttpProtocolIpv6": "disabled",
  "InstanceMetadataTags": "disabled"
}
```

### Set at Launch

```bash
aws ec2 run-instances \
  --image-id ami-0123456789abcdef0 \
  --instance-type t3.micro \
  --metadata-options "HttpPutResponseHopLimit=2"
```

### Modify a Running or Stopped Instance

```bash
aws ec2 modify-instance-metadata-options \
  --instance-id i-0a1b2c3d4e5f67890 \
  --http-put-response-hop-limit 2
```

Raise the hop limit above the default of 1 when containers on the instance need IMDS access through an extra routing hop (e.g. containers on a bridge network fetching role credentials). Valid range is 1–64.

Rejected settings:

- `--http-tokens optional` → `UnsupportedOperation` (IMDSv2 cannot be relaxed)
- `--http-endpoint disabled` → `UnsupportedOperation` (the endpoint cannot be turned off)
- `--http-protocol-ipv6 enabled` → `UnsupportedOperation`
- `--instance-metadata-tags enabled` → `UnsupportedOperation`
- Hop limit outside 1–64 → `InvalidParameterValue`

## How It Works

Useful background for host-side troubleshooting:

- The IMDS HTTP server runs inside the `spinifex-vpcd` service on each host, listening on `169.254.169.254:80` with one listener per instance's primary interface — attribution is structural, with no source-IP trust.
- vpcd reconciles listeners against live interfaces every **15 seconds**, so a freshly launched guest's metadata service is serving within one reconcile tick (cloud-init's built-in retries absorb this window).
- Session tokens and cached role credentials live in memory only. A vpcd restart drops them; SDKs and cloud-init transparently reissue. Datapath state on the host survives service restarts.

## Limits and Defaults

| Setting | Value |
| ------- | ----- |
| Token TTL | 1–21600 seconds (request-scoped, via header) |
| Token binding | Issuing network interface; in-memory, not persisted |
| Role credential lifetime | 3600 seconds, refreshed 5 minutes before expiry |
| PUT response hop limit | Default 1, valid 1–64 |
| `HttpTokens` | Always `required` (immutable) |
| `HttpEndpoint` | Always `enabled` (immutable) |
| Tokenless GET | `401 Unauthorized`, empty body |
| `X-Forwarded-For` present | `403 Forbidden` |
| Wrong method | `405 Method Not Allowed` |
| Unknown/unsupported path | `404 Not Found` |

## Troubleshooting

### 401 Unauthorized on Every Read

The request is missing a valid token. Common causes:

- No `X-aws-ec2-metadata-token` header — IMDSv1-style access is not supported; obtain a token first.
- The token expired — reissue with a fresh `PUT /latest/api/token`.
- The token was issued to a different instance/interface — tokens are interface-bound and rejected elsewhere, identically to unknown tokens.
- The token endpoint itself never 401s; if the `PUT` fails, check for a `400` (bad TTL header) instead.

### 403 Forbidden

The request carried an `X-Forwarded-For` header. IMDS rejects proxied requests outright; call it directly from the workload, not through a forward proxy.

### Software in a Container Cannot Reach IMDS

The default hop limit of 1 means the PUT response dies at the first routed hop, e.g. a Docker bridge network. Raise it from the control plane:

```bash
aws ec2 modify-instance-metadata-options \
  --instance-id i-0a1b2c3d4e5f67890 \
  --http-put-response-hop-limit 2
```

### UnsupportedOperation When Setting Metadata Options

You attempted to relax IMDSv2 enforcement (`--http-tokens optional`) or disable the endpoint (`--http-endpoint disabled`). Neither is supported — the platform posture is fixed. Only the hop limit can be changed.

### IAM Credentials Return 404, an Empty List, or "Code": "Failed"

A 404 or empty list means the instance has no IAM instance profile, or the profile has no role. Verify from the control plane:

```bash
aws ec2 describe-instances --instance-ids i-0a1b2c3d4e5f67890 \
  --query 'Reservations[0].Instances[0].IamInstanceProfile'
```

A credential body with `"Code": "Failed"` means the backend could not mint credentials (for example, the role was deleted after launch). Check the role still exists and the vpcd logs on the host for `IMDS: AssumeRoleForInstance failed`. See the IAM Roles and Instance Profiles guide for setting up profiles.

### Connection Refused / Timeout to 169.254.169.254

Metadata is served per-interface by `spinifex-vpcd` on the host. In order:

1. Immediately after launch, wait a few seconds — the listener converges within one 15-second reconcile tick, and cloud-init retries through it.
2. On the host, confirm vpcd is running and serving:

```bash
systemctl status spinifex-vpcd
journalctl -u spinifex-vpcd | grep 'IMDS:'
```

Look for `IMDS: tap responder serving` (listener bound) and `IMDS: issued IMDSv2 token` (proof guest packets are traversing the datapath). A bound responder with no token issuance points at the host datapath rather than the HTTP service:

```bash
ovs-vsctl list-ports br-imds          # per-instance endpoint + patch ports
ovs-vsctl list-ports br-int | grep imi-
```

### cloud-init Did Not Apply SSH Key or User Data

cloud-init sources both from IMDS at first boot. Check `public-keys/0/openssh-key` and `user-data` respond from inside the guest (see [Using IMDS from a Guest](#using-imds-from-a-guest)), and review `/var/log/cloud-init.log` in the guest. A 404 on `public-keys/` means the instance was launched without `--key-name` or the key pair was since deleted.
