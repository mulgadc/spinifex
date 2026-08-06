---
title: "RDS Quickstart"
description: "Stand up a managed PostgreSQL database on Spinifex with Terraform — a VPC, a DB subnet group, a parameter group, an aws_db_instance, and a client VM inside the VPC that connects to it with psql."
category: "Terraform Workbooks"
tags:
  - terraform
  - rds
  - postgres
  - database
  - vpc
  - workbook
sections:
  - overview
  - prerequisites
  - instructions
  - troubleshooting
resources:
  - title: "RDS CLI Reference"
    url: "https://github.com/mulgadc/spinifex/blob/main/docs/COMMANDS.md#rds-postgresql"
  - title: "Terraform AWS Provider — aws_db_instance"
    url: "https://registry.terraform.io/providers/hashicorp/aws/latest/docs/resources/db_instance"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
  - title: "OpenTofu"
    url: "https://opentofu.org/"
---

# Terraform: RDS Quickstart

> The smallest end-to-end RDS example on Spinifex: a VPC with a public client subnet and a private DB subnet, a DB subnet group, a parameter group, a PostgreSQL DB instance, and a client VM with `psql` already pointed at the endpoint.

## Overview

A Spinifex DB instance runs in a VM you never see. What lands in your VPC is a single **endpoint ENI** in one of the DB subnet group's subnets, so the endpoint is **always private** — there is no `publicly_accessible` mode, and a request for one is rejected. That is why this workbook builds a client VM: it is the only place from which the database can be reached.

The layout is the one that shape implies — the tier that talks to the database in its own subnet, and the database in a subnet with no route off the VPC:

```
     IGW
      │
   client subnet 10.60.1.0/24 ──── client VM
                                       │
                                    psql:5432
                                       │
   db subnet     10.60.2.0/24 ──── endpoint ENI ──▶ DB VM (platform-owned)
```

The endpoint is reachable from **any subnet of the VPC**, so the client does not have to share the subnet the endpoint ENI landed in — which also means a subnet group spanning several subnets works wherever the endpoint is placed within it.

The client subnet is public in that it has an internet-gateway route, which the client needs to `apt-get` a psql. The DB subnet has no route table association, so it stays on the main route table `create-vpc` writes: intra-VPC routing and nothing else. The endpoint ENI gets no public address and `publicly_accessible = true` is rejected either way.

The database security group admits `5432` from the client security group and nothing else, which compiles to an ACL on the endpoint ENI itself — the port is deny-by-default for everything else in the VPC.

## Prerequisites

- **Spinifex running**, with the AWS CLI configured for the `spinifex` profile (see [Installing Spinifex](/docs/install)) and OpenTofu (or Terraform) installed.
- **The `spinifex-rds-postgres` image registered.** DB instances boot from this system image. Import it once per cluster, then verify that all tags used by the RDS engine resolver are present:

  ```bash
  spx admin images import --name spinifex-rds-postgres

  AWS_PROFILE=spinifex aws ec2 describe-images \
    --filters \
      'Name=tag:spinifex:managed-by,Values=rds' \
      'Name=tag:engine,Values=postgres' \
      'Name=tag:engine-version,Values=18' \
    --query 'Images[].[ImageId,Name]' --output text
  ```

- **An Ubuntu image** for the client VM, resolved here by a `*ubuntu-26.04*` / `*ubuntu-24.04*` name filter.
- **Roughly 2 GiB of free guest memory** — one `db.t3.micro` DB VM plus one `t3.small` client.

## Instructions

### 1. Fetch the workbook

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/mulgadc/spinifex.git spinifex-tf
cd spinifex-tf
git sparse-checkout set docs/terraform-workbooks
cd docs/terraform-workbooks/rds-quickstart
```

### 2. Apply

```bash
export AWS_PROFILE=spinifex

tofu init
tofu apply -var 'db_password=<choose-one>'
```

Creating the instance boots a VM, runs `initdb` and waits for the in-guest agent's first healthy heartbeat, so the DB is several minutes of the apply on its own. Terraform waits for `available` before it launches the client, because the client's cloud-init needs the endpoint address.

### 3. Connect

```bash
tofu output ssh_to_client
ssh -i rds-quickstart-client.pem ubuntu@<client_public_ip>
```

`PGHOST`, `PGPORT`, `PGUSER` and `PGDATABASE` are exported from `/etc/profile.d`, and the password is in `~/.pgpass` at `0600` — so `psql` on its own connects:

```bash
psql -c 'SELECT version();'
psql -c 'CREATE TABLE hello (id int primary key, note text);'
psql -c "INSERT INTO hello VALUES (1, 'it works');"
psql -c 'SELECT note FROM hello;'
```

Give the client a minute after apply: `postgresql-client` is installed by cloud-init on first boot.

TLS is offered but not enforced. `psql "sslmode=require"` encrypts the connection. For `sslmode=verify-full`, copy the cluster CA to the client — both the endpoint name and the endpoint IP are in the certificate's SAN set, so either address verifies:

```bash
scp -i rds-quickstart-client.pem /etc/spinifex/ca.pem ubuntu@<client_public_ip>:~/ca.pem
psql "sslmode=verify-full sslrootcert=/home/ubuntu/ca.pem" \
  -c 'SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid();'
```

### 4. Variables

| Variable | Default | Purpose |
|---|---|---|
| `region` | `ap-southeast-2` | AWS region. |
| `name` | `rds-quickstart` | Resource name prefix and the DB instance identifier. |
| `spinifex_endpoint` | `https://127.0.0.1:9999` | Gateway as seen from the host running Terraform. |
| `instance_type` | `t3.small` | EC2 type for the **client** VM. |
| `db_instance_class` | `db.t3.micro` | DB class. One of the curated `db.*` subset. |
| `engine_version` | `18` | PostgreSQL major. `18` is the only version served. |
| `db_name` | `appdb` | Database created at bootstrap. |
| `db_username` | `appuser` | Master user. `postgres`, `rdsadmin`, `rds_superuser` and `pg_*` are reserved. |
| `db_password` | `QuickstartS3cret1` | Master password — override it. No `/`, `"`, `@` or spaces. |
| `allocated_storage` | `20` | Data-volume GiB. Grow-only, and a grow is stop/start. |

### 5. Teardown

```bash
tofu destroy -var 'db_password=<the-one-you-used>'
```

The instance is created with `skip_final_snapshot = true` on purpose. A final snapshot **pins the data volume alive** until that snapshot is deleted, so a workbook that took one would leave storage behind on every destroy. For anything you care about, take one:

```bash
aws rds delete-db-instance --db-instance-identifier rds-quickstart \
  --final-db-snapshot-identifier rds-quickstart-final
```

## Things this workbook does deliberately

- **`storage_encrypted = true` is set explicitly.** Storage is always encrypted, so the instance reports `true` whether or not you asked. The provider's attribute is optional but *not* computed, so leaving it out means the read-back carries a value your configuration does not — and every subsequent `plan` shows a change on an instance nothing has touched.
- **`instance_class` and `engine_version` are literal.** `describe-db-engine-versions` and `describe-orderable-db-instance-options` are not implemented, so the `aws_rds_engine_version` and `aws_rds_orderable_db_instance` data sources are unavailable. `aws_db_instance` needs neither.
- **A DB subnet group over one subnet.** AWS requires a DB subnet group to span two AZs. Spinifex is single-AZ — every subnet reports the same zone — so the group accepts any count, and a second private subnet here would buy nothing but a second CIDR.
- **The password is written to `~/.pgpass`, not to the environment.** A `PGPASSWORD` in `/etc/profile.d` is readable by every user on the client and leaks into `ps` output.

## Troubleshooting

**`apply` fails resolving the DB AMI.** The `spinifex-rds-postgres` image is not registered with the required engine tags. Run `spx admin images import --name spinifex-rds-postgres`, then repeat the tagged `describe-images` check in Prerequisites before applying again.

**The apply sits on `aws_db_instance.main: Still creating...`.** Several minutes is normal — a VM boot plus `initdb` plus the first healthy heartbeat. Much longer is a bootstrap that did not finish:

```bash
aws rds describe-db-instances --db-instance-identifier rds-quickstart \
  --query 'DBInstances[0].[DBInstanceStatus,StatusInfos[0].Message]' --output text
aws rds describe-events --source-type db-instance --source-identifier rds-quickstart
```

**`psql` hangs on the client.** Three causes, in the order worth checking:

- cloud-init has not finished installing `postgresql-client` — check `cloud-init status --long`.
- the security group is not letting you through. Nothing outside the client security group can open `5432` — that is the point of the rule — so a psql from your workstation will always time out.
- the client is outside the VPC. The endpoint exists only inside it: any subnet will do, but there is no path to it from anywhere else.

**`InsufficientInstanceCapacity` on the DB instance.** The node admits a launch against live free memory. Free some, or drop to a smaller `db_instance_class`.

**`tofu destroy` leaves a DB instance behind.** `deletion_protection` blocks a delete outright. This workbook sets it to `false`; if you turned it on, clear it before destroying:

```bash
aws rds modify-db-instance --db-instance-identifier rds-quickstart \
  --no-deletion-protection --apply-immediately
```
