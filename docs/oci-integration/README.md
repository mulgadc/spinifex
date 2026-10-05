---
title: "Oracle Cloud Infrastructure (OCI)"
seoTitle: "Run Spinifex on Oracle Cloud Infrastructure — Spinifex Docs"
description: "Deploy Spinifex on Oracle Cloud Infrastructure end to end with Terraform, on one node or three, and give guests real public addresses through OCI's own API."
category: "Cloud Install"
tags:
  - install
  - oci
  - oracle cloud
  - terraform
  - cluster
resources:
  - title: "OCI Architecture and Operations"
    url: "/docs/oci-architecture"
  - title: "Multi-Node Install"
    url: "/docs/install-multi-node"
  - title: "Host Firewall"
    url: "/docs/host-firewall"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
---

# Running Spinifex on Oracle Cloud Infrastructure

> Run the AWS surface (EC2, EBS, S3, VPC) inside your own OCI tenancy, with guests that get real, publicly reachable addresses through OCI's API.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [1. Clone the Spinifex repository](#1-clone-the-spinifex-repository)
- [2. Give the nodes a limited scoped OCI credential](#2-give-the-nodes-a-limited-scoped-oci-credential)
- [3. Choose deployment method](#3-choose-deployment-method)
- [4. Write your Terraform inputs](#4-write-your-terraform-inputs)
- [5. Deploy](#5-deploy)
- [6. Check it worked](#6-check-it-worked)
- [7. Set up your cluster](#7-set-up-your-cluster)
- [Harden it before production](#harden-it-before-production)
- [Next steps](#next-steps)
- [Removing the deployment](#removing-the-deployment)
- [Troubleshooting](#troubleshooting)

---

## Overview

Spinifex is an open-source infrastructure platform that brings core AWS services to bare-metal, edge, and on-prem environments. It serves the EC2, EBS, S3, VPC, IAM and STS APIs over a SigV4 endpoint of your own, so the AWS CLI, the AWS SDKs and the AWS Terraform provider all work against it unmodified. Nothing a tenant has written has to be rewritten to run on it.

This guide installs Spinifex on Oracle Cloud Infrastructure, on one instance or three, with guests that get real public addresses from OCI's own pool. The install is the same product you would put on your own servers, so [Single-Node Install](/docs/install) and [Multi-Node Install](/docs/install-multi-node) describe the same formation, storage and networking. What differs here is the layer underneath, because a VCN is not an Ethernet segment and public addresses come from an API rather than a range you choose.

**Why run it on OCI:**

- **Cost.** OCI prices compute and block storage below the large US clouds, and one Spinifex node serves a tenant's whole fleet as QEMU guests on hardware you are billed for once.
- **Egress.** Oracle's outbound-transfer allowance and per-GB rate are far cheaper than the majors, and guest-to-guest traffic rides the private plane, where it is not billed as internet egress at all.
- **Portability.** Your tenants code against Spinifex, not against OCI, so the same workloads move to your own rack or an edge site later as a deployment decision rather than a rewrite.
- **Real public addresses.** Spinifex registers each guest's address with OCI, so a guest is reachable on the internet rather than hidden behind a shared NAT.

**What gets installed**, on every node:

- Spinifex daemon and CLI
- QEMU/KVM for guests
- OVN and Open vSwitch for VPC networking
- [Predastore](https://github.com/mulgadc/predastore), S3-compatible object storage
- [Viperblock](https://github.com/mulgadc/viperblock), EBS-compatible block storage
- The OCI public-address allocator, which is the one component specific to this platform

Nodes run **Ubuntu 26.04**, resolved at plan time as the newest Canonical platform image for your shape and region. Oracle Linux support as the host OS to run Spinifex is under active development.

**Terraform does the whole deployment:** the VCN, the block volumes, both VNICs per node, the Spinifex install, the cluster formation and the allocator. Six steps below, one command, about ten minutes for a single node.

What sits underneath is in [Architecture and Operations](../oci-architecture/README.md), which also carries the IAM policy, the quotas, every variable and the troubleshooting.

## Prerequisites

|                                            |                                                                                                                                         |
| ------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------- |
| **An OCI compartment OCID**                | This deploys into a compartment that already exists. Creating one needs tenancy-root rights the API user must not have                  |
| **OCI credentials in `~/.oci/config`**     | For Terraform, to build the infrastructure. `oci setup config` writes one, or write it by hand                                          |
| **An OCI API key for the nodes**           | Separate from the above, and the subject of step 2. Without it a node comes up healthy and still cannot give any guest a public address |
| **Terraform or OpenTofu, `git`, Python 3** | On your workstation. The Python helper is standard library only, so there is nothing to `pip install`                                   |

Check two quotas before you start, because both refuse at apply time rather than at plan time. **Reserved public IPs** are capped at 50 per region across the whole tenancy, and the **compute limit for your shape** starts at zero for bare metal on many new tenancies. [Quotas](../oci-architecture/README.md#quotas) has the commands.

For the workstation (Linux, MacOS, Windows [WSL]) that will be used to deploy Spinifex on OCI the following prerequisites are required.

### OCI CLI tool

Ensure the `oci` CLI tool is pre-installed. Follow the install guide at: [https://docs.oracle.com/en-us/iaas/Content/API/Concepts/cliconcepts.htm](https://docs.oracle.com/en-us/iaas/Content/API/Concepts/cliconcepts.htm)

Once installed configure your OCI credentials (~/.oci/config) which will be used in later steps in the installation tutorial.

`oci setup config`

Once setup validate your OCI credentials work as expected. `$CID` is the compartment you are deploying into, which is a `ocid1.compartment.oc1..` OCID, or your tenancy OCID if that compartment is the root. The same value is used throughout this guide and again as `compartment_ocid` in step 4.

```sh
export CID="ocid1.compartment.oc1..xxx"
oci compute shape list --compartment-id $CID
```

Expected output to confirm API key works as expected:

```json
{
  "data": [
    {
      "baseline-ocpu-utilizations": null,
      "billing-type": "PAID",
      ...
    }
  ]
}
```

### Install Terraform / OpenTofu

The next prerequisite is to install Terraform or OpenTofu to deploy the Spinifex stack on OCI.

- Terraform - [https://developer.hashicorp.com/terraform/install](https://developer.hashicorp.com/terraform/install)
- OpenTofu - [https://opentofu.org/docs/intro/install/](https://opentofu.org/docs/intro/install/)

## Instructions

## 1. Clone the Spinifex repository

To begin the deployment of Spinifex on OCI deploy the repository:

```bash
git clone https://github.com/mulgadc/spinifex.git
cd spinifex
git switch dev
cd scripts/terraform/oci-spx
```

> [!IMPORTANT]
> **`git switch dev` is needed today.** OCI support is not in the current release, so the deploy driver is not on `main` yet. From the next release onward `main` carries it and you can skip that line. This chooses the _Terraform and the driver_ only. Which Spinifex build lands on the nodes is step 5.

Then make a new SSH keypair the deploy installs on every Spinifex node which you can login with:

```bash
ssh-keygen -t ed25519 -f ~/.ssh/oci-spx -N '' -C spinifex-oci
```

Terraform reads your OCI credential from `~/.oci/config` which is a prerequisites step above, taking the tenancy, the user, the fingerprint, the key and **the region** from the profile. A config holding a single `[DEFAULT]` profile, which is what `oci setup config` writes, needs nothing further.

Optionally with more than one profile you can define one using the environment variable `OCI_CLI_PROFILE`

```bash
export OCI_CLI_PROFILE=customprofile
```

## 2. Give the nodes a limited scoped OCI credential

Spinifex calls the OCI API at runtime to register each guest's public address and related functions for Spinifex to support running within an OCI environment. This step gives Spinifex a credential with a limited permission scope for improved security.

### Generate the key

```bash
openssl genrsa -out ~/.oci/oci_api_key_spx.pem 2048
chmod 600 ~/.oci/oci_api_key_spx.pem
openssl rsa -pubout -in ~/.oci/oci_api_key_spx.pem -out ~/.oci/oci_api_key_spx_public.pem
openssl rsa -pubout -outform DER -in ~/.oci/oci_api_key_spx.pem | openssl md5 -c
```

The last command prints the **fingerprint**, which is how OCI names a key everywhere it refers to one. Keep it; you need it in the config file at the end of this step.

### Create a user and a group for it

Both of these are identity resources, so they are created in the tenancy rather than in your compartment. This step requires the prerequisite `oci` CLI tool to be installed and configured with an API scope that includes creating new user accounts and groups.

Replace `admin@yourdomain.com` with your administrator email which will be linked to the new OCI user account.

```bash
export ADMIN_EMAIL=admin@yourdomain.com

USER_OCID=$(oci iam user create --name spinifex-allocator \
    --email $ADMIN_EMAIL \
    --description "Spinifex external-address allocator" \
    --query 'data.id' --raw-output)

GROUP_OCID=$(oci iam group create --name SpinifexOperators \
    --description "May manage the private and public IPs behind Spinifex external addresses" \
    --query 'data.id' --raw-output)

oci iam group add-user --group-id "$GROUP_OCID" --user-id "$USER_OCID"
```

On success the output will include:

```json
{
  "data": {
    "compartment-id": "ocid1.tenancy.oc1..xxx",
    "group-id": "ocid1.group.oc1..xxx",
    "id": "ocid1.groupmembership.oc1..axxx",
    "inactive-status": null,
    "lifecycle-state": "ACTIVE",
    "time-created": "2026-10-05T19:39:48.478000+00:00",
    "user-id": "ocid1.user.oc1..xxx"
  }
}
```

An email will be sent to the designated `$ADMIN_EMAIL` defined, you are required to click the "Activate Your Account" link to fully activate the new account in OCI.

> [!NOTE]
> **If these fail with `NotAuthorizedOrNotFound`, you do not have identity rights in the tenancy**, which is normal when someone allocated you a compartment inside theirs. Ask whoever owns the tenancy to grant you the required permissions.

### Grant the group its three permissions

Spinifex calls [ten operations](../oci-architecture/README.md#the-iam-policy), and these three statements are what authorise them. Attach the policy to the compartment you are deploying into using the command below. Replace `$CID` with that compartment's OCID.

```bash
export CID="ocid1.compartment.oc1..xxx"

oci iam policy create --compartment-id "$CID" --name SpinifexOperators \
    --description "Spinifex external-address allocation" \
    --statements '[
      "Allow group SpinifexOperators to use vnics in compartment id '"$CID"'",
      "Allow group SpinifexOperators to manage private-ips in compartment id '"$CID"'",
      "Allow group SpinifexOperators to manage public-ips in compartment id '"$CID"'"
    ]'
```

> [!NOTE]
> **If this fails with `NotAuthorizedOrNotFound`, confirm `$CID` is the compartment you are deploying into.** It is a `ocid1.compartment.oc1..` OCID, or your tenancy OCID if you deploy into the root compartment. An identity-domain OCID is a different resource and will not work here.

### Upload the public key to that user

```bash
oci iam user api-key upload --user-id "$USER_OCID" \
    --key-file ~/.oci/oci_api_key_spx_public.pem
```

This will output:

```json
{
  "data": {
    "fingerprint": "xxx",
    "inactive-status": null,
    "key-id": "ocid1.tenancy.oc1..xxx",
    "key-value": "-----BEGIN PUBLIC KEY-----xxxx-----END PUBLIC KEY-----",
    "lifecycle-state": "ACTIVE",
    "time-created": "2026-10-05T19:51:58.843000+00:00",
    "user-id": "ocid1.user.oc1..axxx"
  },
  "etag": "xxx"
}
```

### Write the profile and prove it works

The Spinifex nodes authenticate with an OCI SDK config under the profile name `spinifex`. Write that profile on your workstation first, because it is both what the nodes get and the only way to test the credential before a deployment depends on it:

```ini
# ~/.oci/config
[spinifex]
user=<the USER_OCID from above>
fingerprint=<the fingerprint from above>
tenancy=<your tenancy OCID, the same one your DEFAULT profile uses>
region=<your region, e.g. ap-sydney-1>
key_file=~/.oci/oci_api_key_spx.pem
```

Then confirm the key, the user, the group and the policy all line up. This call needs exactly the `public-ips` grant above, so it fails in the same way a node would:

```bash
oci network public-ip list --compartment-id "$CID" --scope REGION --all --profile spinifex
```

An empty response is a pass, because the permission is what is being tested rather than the contents. `NotAuthorizedOrNotFound` means the policy is not in force: check that the statements name the compartment by `compartment id`, and that the user is actually in the group.

**Do this before step 5.** confirm the new OCI account can correctly authenticate and issue the required API calls for Spinifex to function correctly once installed.

> [!WARNING]
> **Do not set `OCI_CLI_PROFILE=spinifex`.** That variable chooses the profile **Terraform** builds the infrastructure with, and this one cannot: creating a VCN, a subnet and instances is far outside the three grants above, so the apply fails on the first resource. The nodes find the `spinifex` profile by name without being told.

### Validate deployment

Prior to running the Terraform scripts to deploy Spinifex, verify the OCI configuration and environment is correctly setup:

```bash
./spx-oci-config.sh --dry-run ~/.ssh/oci-spx
```

Expected output:

```text
[spx-oci-config] credential: the [spinifex] profile in ~/.oci/config
[spx-oci-config] user: ocid1.user.oc1..aaaa...
[spx-oci-config] fingerprint: a1:b2:...
[spx-oci-config] region: ap-sydney-1
[spx-oci-config] would write /etc/spinifex/oci/config and /etc/spinifex/oci/oci_api_key.pem as 0640 root:spinifex on: (no hosts given)
```

Step 5 runs this script for real, over SSH, once the nodes are formed. Those two files are what the allocator authenticates with, and the `spinifex` profile name in the second one is what it looks for.

**The `credential:` line is the one to check.** It has to name the `[spinifex]` profile. If instead it prints two `WARNING:` lines about falling back to "the credential Terraform builds with", the profile was not found and your nodes would get your full-access credential rather than the scoped one you just made.

> [!TIP]
> **A tenancy admin can skip key files altogether** and authenticate the nodes as the instance itself, which leaves no credential on disk to rotate or leak. It needs a dynamic group and a policy created once at the tenancy root, so it is not available in a compartment someone allocated to you. [Instance principal](../oci-architecture/README.md#an-instance-principal-no-key-material-but-it-needs-a-tenancy-admin) covers it, and you then pass `--instance-principal` in step 5 instead of `--credential-hook`.

## 3. Choose deployment method

Choose how to deploy Spinifex on OCI, either as a single node, or at minimum a three node cluster for added resiliency and redundancy.

|                                | Single node                                  | Three nodes                                                                                     |
| ------------------------------ | -------------------------------------------- | ----------------------------------------------------------------------------------------------- |
| **`node_count`**               | `1`                                          | `3`                                                                                             |
| **Survives losing a node**     | No                                           | Yes. NATS keeps quorum, predastore RS(2,1) keeps the data readable, the OVN raft keeps a leader |
| **Public addresses available** | 64                                           | 192, three VNICs' worth, still bounded by the regional quota                                    |
| **Fault domains**              | One                                          | Three, one per node, chosen by Terraform                                                        |
| **Use it for**                 | Evaluation, a lab, an edge site with one box | Production. Multi-node resiliency                                                               |

## 4. Write your Terraform inputs

One file, in the directory you are already in. Only `compartment_ocid` is required, because every other variable has a default that builds a working single node:

Recommended compute shapes for a VM deployment:

- `VM.Standard.E6.Flex` - Higher performance, 5th Gen AMD EPYC processor
- `VM.Standard.E5.Flex` - Base-line performance, 4th Gen AMD EPYC processor

Recommended bare metal shapes, preferred for large-scale production:

- `BM.Standard.E5.192` - 192 OCPU / 2304 GB RAM. Higher performance, 4th Gen AMD EPYC processor
- `BM.Standard3.64` - 64 OCPU / 1024 GB RAM. Baseline, Intel Xeon Platinum 8358
- `BM.Standard.E2.64` - 64 OCPU / 512 GB RAM. Legacy, 1st Gen AMD EPYC processor

```bash
cat > terraform.auto.tfvars <<EOF
compartment_ocid        = "$CID"
deployment_name         = "spinifex"

# Shape and size.
# Bare metal is preferred for large scale production usage.
compute_shape           = "VM.Standard.E5.Flex"
compute_ocpus           = 8          # OCI counts an OCPU as a full core, 1 OCPU = 2vCPU
compute_memory_in_gbs   = 32

# 1 for a single node, 3 required for a cluster.
node_count              = 1

# The block volume behind /var/lib/spinifex, attached over iSCSI.
data_volume_size_in_gbs = 256
data_volume_vpus_per_gb = 120        # 120 = Ultra High Performance

# Firewall. A list, so it can hold several ranges. Read the note below before
# narrowing it.
node_client_cidr_allow_list = ["0.0.0.0/0"]
EOF
```

The heredoc delimiter is unquoted, which is what lets `$CID` expand from the variable you exported in step 2. Check the file rather than assume, because a literal `$CID` in there fails at plan time with an unhelpful message about an invalid OCID:

```bash
grep compartment_ocid terraform.auto.tfvars
```

**This file is what decides the deployment.** `compute_ocpus` and `compute_memory_in_gbs` size a VM flex shape and are ignored on a fixed bare-metal one, which comes at one fixed size with all available OCPU/memory.

[Every Terraform variable](../oci-architecture/README.md#every-terraform-variable) has the full list.

> [!NOTE]
> **Leave `node_client_cidr_allow_list` at its `0.0.0.0/0` default for now.** It is an OCI security-list rule covering the whole public subnet, and your guests' public addresses cross it too, so narrowing it to your own address cuts internet access to every guest rather than just to the node. [Harden it before production](#harden-it-before-production) locks the node down in the layer that can tell the two apart.

## 5. Deploy

```bash
./validate-topology.sh \
    --topology vm-single \
    --channel dev \
    --credential-hook ./spx-oci-config.sh \
    --skip-workload \
    --keep
```

> [!IMPORTANT]
> **`--channel dev` is needed today.** OCI support is not in a published release yet, so the current release cannot allocate an OCI public address. From the next release onward, drop `--channel dev` and the deploy takes `latest`.

One command builds the infrastructure, installs Spinifex, forms the cluster, configures OCI public addressing and verifies that the address allocator came up.

| Argument            | What to pass                                                                                                                                                                                                                                                  |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--topology`        | `vm-single` for one VM, `vm-multi` for three, `bm` for one bare-metal host. Required, with no default, so a command cannot be aimed at the wrong one by omission. It supplies a shape and a node count only where your tfvars is silent                       |
| `--channel`         | **`dev` until the next release ships**, because OCI support is not in the current stable release yet. It resolves to the newest pre-release at the moment you run it. `latest` is the default and becomes the right answer once a release carries OCI support |
| `--credential-hook` | `./spx-oci-config.sh`, which installs the credential from step 2 on every node. With an instance principal, replace it with `--instance-principal` and there is no key to install                                                                             |
| `--skip-workload`   | Stop once the cluster is verified. Drop it and the driver also runs the published Terraform workbooks on the node, which launches real guests on public addresses and tears each one down again                                                              |
| `--keep`            | **This is what makes it a deployment rather than a test.** Without it the driver destroys everything at the end, which is right for CI and wrong here                                                                                                         |

**Your `terraform.auto.tfvars` outranks the topology**, so `--topology bm` with `compute_shape = "BM.Standard.E5.192"` in that file deploys on that shape rather than the one CI runs. The run logs the shape and node count it actually built, straight after the host addresses.

Add `--dry-run` to print the plan and change nothing.

> [!NOTE]
> **If the install step fails to download, pass a tag instead of the channel.** `dev` is the one channel that resolves through an unauthenticated GitHub API call, so it can be rate-limited and then 404s. Take the newest tag from the [releases page](https://github.com/mulgadc/spinifex/releases) and swap `--channel dev` for `--version <tag>`, which resolves by redirect and cannot trip the same limit. A tag is also what to use for a repeatable deployment, where you want the build pinned rather than current.

## 6. Check it worked

The stages print as they finish, and all of these have to appear:

```text
[validate-vm-single] building
[validate-vm-single] waiting for cloud-init on each node
[validate-vm-single] installing Spinifex on each node
[validate-vm-single] initializing the single node
[validate-vm-single] running the credential hook
[validate-vm-single] credential hook ok
[validate-vm-single] configuring the IMDS remap and the external pool
[validate-vm-single] checking the allocator came up
[validate-vm-single] 150.230.13.131 allocator ready
[validate-vm-single] cluster membership
[validate-vm-single] --skip-workload: stopping before the workbook
[validate-vm-single] --keep: leaving the infrastructure up, no verdict recorded
```

**`allocator ready` is the line to look for.** Every line above it also prints on a node that cannot allocate a public address, and that failure stays silent until a guest asks for one.

`running the credential hook` is `spx-oci-config.sh`, and its own output is captured rather than printed. If the deploy stops there, read `credential-hook.log` in the state directory the driver names at startup — its `credential:` line says which credential went onto the nodes.

### Access the Spinifex node

SSH to the node at the address on the `allocator ready` line:

```bash
ssh -i ~/.ssh/oci-spx ubuntu@<node-ip>
```

Then call the AWS surface with the `[spinifex]` profile that `spx admin init` wrote into `~/.aws/credentials`:

```bash
export AWS_PROFILE=spinifex
aws ec2 describe-instance-types
```

```text
{
    "InstanceTypes": [
        {
            "InstanceType": "c7a.12xlarge",
            "CurrentGeneration": true,
        ...
        }
    ]
}
```

If this returns a list of available instance types, your installation is working.

**Congratulations! Spinifex is installed.**

## 7. Set up your cluster

Spinifex is running, but it holds nothing yet — no machine images, no networks, no instances.

Continue to [Setting Up Your Cluster](/docs/setting-up-your-cluster) to import an AMI, create an SSH key pair, create a VPC with a public subnet, and launch your first instance.

> [!IMPORTANT]
> **Probe a guest's public address from your workstation, not from the node.** A node is a poor vantage point for its own network's public addresses, so a timeout measured there says nothing about the guest. Private addresses are a separate question, and those do work from inside.

## Harden it before production

The deployment comes up usable, not locked down. Three layers filter traffic here and the first job is telling them apart, which [Firewalls, in all three layers](../oci-architecture/README.md#firewalls-in-all-three-layers) does in full. The short version:

**1. Arm the Spinifex host firewall.** It is installed but not armed on this path. Arming it gives you the proper policy: the public plane open, the cluster plane scoped to peers only, and SSH scoped to a list you own.

```bash
# On every node.
sudo env SETUP_STAGES=firewall /usr/local/share/spinifex/setup.sh --firewall on
```

**2. Narrow SSH.** It is open to the world until you do, which comes from the Ubuntu image's own rules rather than from ours. Edit the one file the installer creates and never overwrites:

```bash
# /etc/spinifex/firewall/custom.nft
define trusted_ssh_peers = { 203.0.113.10/32 }
```

```bash
sudo nft -c -f /etc/spinifex/firewall/spinifex.nft     # check, then apply
sudo /usr/local/lib/spinifex/spinifex-firewall-apply
```

**3. Decide about the console on port 3000.** The S3 gate (8443) and the AWS gateway (9999) are the product's public surface and should stay public. The web console need not be.

**Leave the OCI security list open.** Guest public addresses cross it, so narrowing `node_client_cidr_allow_list` breaks your tenants instead of protecting the node. The host firewall above is the layer that can tell a node's listener from a guest's.

Full detail, including which port serves what and why forming a cluster starts by turning the policy off, is in [Host Firewall](/docs/host-firewall).

## Next steps

**Set up your cluster.** It is running but holds nothing yet: no images, no networks, no instances. [Setting Up Your Cluster](/docs/setting-up-your-cluster) imports an AMI, creates a key pair and a VPC, and launches your first instance.

**Run your existing Terraform against it.** Everything above builds infrastructure _on_ OCI with the OCI provider. From here you use the **AWS** provider, unmodified from the registry, pointed at your own cluster. Your `aws_vpc`, `aws_instance`, `aws_db_instance`, `aws_ecs_service` and `aws_eks_cluster` resources stay as they are in git. Three things differ, and only three:

```hcl
provider "aws" {
  region     = "ap-southeast-2"
  access_key = var.access_key
  secret_key = var.secret_key

  endpoints {
    ec2 = var.spinifex_endpoint      # https://<node private IP>:9999
    s3  = var.predastore_endpoint    # https://<node private IP>:8443
    iam = var.spinifex_endpoint
    sts = var.spinifex_endpoint
  }

  s3_use_path_style       = true
  skip_metadata_api_check = true
  skip_region_validation  = true
}
```

> **Use the node's private address, not its public one.** The node certificate carries no SAN for the public address, because that address is never on the wire: OCI NATs it to a private one. A call to `https://<public IP>:9999` fails TLS verification with _hostname doesn't match_. Run Terraform from a node, or from anything else inside the VCN.

The [Terraform workbooks](../terraform-workbooks/nginx-alb/README.md) we ship are the worked examples. `e2e-cloudvendor-nightly` runs them on a single VM, on three VMs and on bare metal, publishing a table per topology on its own run page, so read that for the build you are installing.

**Oracle Linux guests.** Four Oracle Linux images are in the catalog, so you can run the distro your Oracle support contract covers:

```bash
sudo spx admin images import --name oracle-10.1-x86_64 --config /etc/spinifex/spinifex.toml
```

The four are `oracle-10.1-x86_64`, `oracle-10.1-arm64`, `oracle-9.8-x86_64` and `oracle-9.8-arm64`. All of them boot **UEFI**, so launch them into a shape that boots UEFI or the failure looks like a hung boot rather than a rejected image. **Log in as `cloud-user`, not `opc`.** These are the generic KVM cloud images from `yum.oracle.com`, which refuse `opc`, `oracle` and `ec2-user` alike. The UI's instance detail page shows the right user per AMI.

## Removing the deployment

```bash
./validate-topology.sh --topology vm-single --destroy-only
```

It destroys whatever that topology's Terraform state holds, so it can only remove what this configuration created.

## Troubleshooting

The deploy stops at the first failure and names the log it wrote, all under `.validate-<topology>/`. Add `--keep-on-fail` to leave a failed deployment up so you can log in and look. **It keeps billing** until you run `--destroy-only`.

[Troubleshooting](../oci-architecture/README.md#troubleshooting) covers the symptoms worth knowing in advance, several of which point at the wrong layer on first reading:

- [If a deploy stage fails](../oci-architecture/README.md#if-a-deploy-stage-fails), and which log holds the answer
- [A guest's public address is unreachable](../oci-architecture/README.md#a-guests-public-address-is-unreachable)
- [A node that was fine and then was not](../oci-architecture/README.md#a-node-that-was-fine-and-then-was-not), where one command answers all of it
- [Guests cannot reach instance metadata](../oci-architecture/README.md#guests-cannot-reach-instance-metadata), because OCI and AWS guests want the same address
- [Addresses and quotas](../oci-architecture/README.md#addresses-and-quotas), including why a detached address still bills

Installing by hand instead of with Terraform is [Deploying without Terraform](../oci-architecture/README.md#deploying-without-terraform).
