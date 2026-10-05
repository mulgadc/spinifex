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
- [1. Get the configuration](#1-get-the-configuration)
- [2. Give the nodes an OCI credential](#2-give-the-nodes-an-oci-credential)
- [3. Choose one node or three](#3-choose-one-node-or-three)
- [4. Write your Terraform inputs](#4-write-your-terraform-inputs)
- [5. Deploy](#5-deploy)
- [6. Check it worked](#6-check-it-worked)
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

[Why run Spinifex on OCI](../oci-architecture/README.md#why-run-spinifex-on-oci) makes the case in full.

**What gets installed**, on every node:

- Spinifex daemon and CLI
- QEMU/KVM for guests
- OVN and Open vSwitch for VPC networking
- [Predastore](https://github.com/mulgadc/predastore), S3-compatible object storage
- [Viperblock](https://github.com/mulgadc/viperblock), EBS-compatible block storage
- The OCI public-address allocator, which is the one component specific to this platform

Nodes run **Ubuntu 26.04**, resolved at plan time as the newest Canonical platform image for your shape and region. Spinifex also supports Debian 13, but OCI publishes no Debian platform image, so this path is Ubuntu only.

**Terraform does the whole deployment:** the VCN, the block volumes, both VNICs per node, the install, the cluster formation and the allocator. Six steps below, one command, about ten minutes for a single node.

What sits underneath is in [Architecture and Operations](../oci-architecture/README.md), which also carries the IAM policy, the quotas, every variable and the troubleshooting.

## Prerequisites

| | |
| --- | --- |
| **An OCI compartment OCID** | This deploys into a compartment that already exists. Creating one needs tenancy-root rights the API user must not have |
| **OCI credentials in `~/.oci/config`** | For Terraform, to build the infrastructure. `oci setup config` writes one, or write it by hand |
| **An OCI API key for the nodes** | Separate from the above, and the subject of step 2. Without it a node comes up healthy and still cannot give any guest a public address |
| **Terraform or OpenTofu, `git`, Python 3** | On your workstation. The Python helper is standard library only, so there is nothing to `pip install` |

Check two quotas before you start, because both refuse at apply time rather than at plan time. **Reserved public IPs** are capped at 50 per region across the whole tenancy, and the **compute limit for your shape** starts at zero for bare metal on many new tenancies. [Quotas](../oci-architecture/README.md#quotas) has the commands.

## Instructions

## 1. Get the configuration

```bash
git clone https://github.com/mulgadc/spinifex.git
cd spinifex
git switch dev
cd scripts/terraform/oci-spx
```

> [!IMPORTANT]
> **`git switch dev` is needed today.** OCI support is not in the current release, so the deploy driver is not on `main` yet. From the next release onward `main` carries it and you can skip that line. This chooses the _Terraform and the driver_ only. Which Spinifex build lands on the nodes is step 5.

Then make the keypair the deploy installs on every node and logs in with. It refuses to start without one, rather than building instances you cannot reach:

```bash
ssh-keygen -t ed25519 -f ~/.ssh/oci-spx -N '' -C spinifex-oci
```

Terraform reads your OCI credential from `~/.oci/config`, taking the tenancy, the user, the fingerprint, the key and **the region** from the profile. So the region you deploy into is the profile's, not something you set in step 4. A config holding a single `[DEFAULT]` profile, which is what `oci setup config` writes, needs nothing further. With more than one profile, name the one you mean:

```bash
./validate-topology.sh --oci-profile mycorp ...      # or export OCI_CLI_PROFILE=mycorp
```

A name you pass that the config does not hold is an error listing the profiles it does hold. It never falls back, because the failure worth preventing is building a cluster in whichever tenancy happened to be first.

## 2. Give the nodes an OCI credential

Spinifex calls the OCI API at runtime to register each guest's public address. Create an API key for that, if you do not already have one:

```bash
openssl genrsa -out ~/.oci/oci_api_key.pem 2048
chmod 600 ~/.oci/oci_api_key.pem
openssl rsa -pubout -in ~/.oci/oci_api_key.pem -out ~/.oci/oci_api_key_public.pem
openssl rsa -pubout -outform DER -in ~/.oci/oci_api_key.pem | openssl md5 -c   # the fingerprint
```

Upload the public key under **Identity → Users → API Keys**, and grant it no more than the [ten operations Spinifex uses](../oci-architecture/README.md#the-iam-policy) in your compartment.

**The deploy installs the credential on each node by calling a small script of yours.** No key material goes into user-data or Terraform state, where it would be readable from instance metadata for the life of the instance. Write a script that puts these two files on each host it is given:

| Path | Mode | Contents |
| --- | --- | --- |
| `/etc/spinifex/oci/oci_api_key.pem` | `0640 root:spinifex` | The private key |
| `/etc/spinifex/oci/config` | `0640 root:spinifex` | An OCI SDK config, profile **`[spinifex]`**, naming the key by path |

The profile name must be `spinifex`. Your script is called as `your-hook <ssh-key> <host>...`, where the first argument is the private key to reach the nodes with and the rest are the hosts. It must exit non-zero if any host failed, so a missing credential stops the deploy instead of surfacing days later as a launch that cannot get an address. [Credentials](../oci-architecture/README.md#credentials-an-api-key-or-an-instance-principal) has the config file's exact contents.

`chmod +x` it. The deploy checks that before it builds anything, so a hook that is missing or not executable costs you a few seconds rather than forty minutes and a bare-metal bill.

**Nothing in this repository ships that script, deliberately.** A credential belongs to whoever owns it, so the hook is yours to write and yours to keep.

> [!TIP]
> A tenancy admin can skip key files entirely and authenticate the nodes as the instance itself. That needs a dynamic group and a policy created once at the tenancy root, covered under [instance principal](../oci-architecture/README.md#an-instance-principal-no-key-material-but-it-needs-a-tenancy-admin). The API key path above works in any tenancy, including a compartment someone allocated to you.

## 3. Choose one node or three

**This is the only architectural decision, and it is one line in step 4.**

| | Single node | Three nodes |
| --- | --- | --- |
| **`node_count`** | `1` | `3` |
| **Survives losing a node** | No | Yes. NATS keeps quorum, predastore RS(2,1) keeps the data readable, the OVN raft keeps a leader |
| **Public addresses available** | 64 | 192, three VNICs' worth, still bounded by the regional quota |
| **Fault domains** | One | Three, one per node, chosen by Terraform |
| **Use it for** | Evaluation, a lab, an edge site with one box | Anything you would be unhappy to lose |

Two nodes is the one answer not worth taking: it doubles the cost of a single node and buys you a cluster that cannot form a quorum. [Sizing](../oci-architecture/README.md#sizing) covers shapes, where bare metal is the recommendation and `VM.Standard.E6.Flex` the minimum.

The pool has no configured size. Spinifex asks OCI for an address when a guest needs one, and **two ceilings bind it, neither of them ours**. The first is 64 secondary private IPs per VNIC, which is the 64 in the table and is not raisable. The second is the regional reserved-public-IP quota, which is tenancy-wide and shared with everything else you run on OCI. You will hit that one first.

## 4. Write your Terraform inputs

One file, in the directory you are already in. Only `compartment_ocid` is required, because every other variable has a default that builds a working single node:

```bash
cat > terraform.auto.tfvars <<'EOF'
compartment_ocid        = "ocid1.compartment.oc1..aaaa..."
deployment_name         = "spinifex"

# Shape and size. Bare metal is preferred; a flex VM is what the quota allows.
compute_shape           = "VM.Standard.E6.Flex"
compute_ocpus           = 8          # OCI counts an OCPU as a full core
compute_memory_in_gbs   = 32

# 1 for a single node, 3 for a cluster. This is the only line that chooses.
node_count              = 1

# The block volume behind /var/lib/spinifex, attached over iSCSI.
data_volume_size_in_gbs = 256
data_volume_vpus_per_gb = 120        # 120 = Ultra High Performance
EOF
```

[Every Terraform variable](../oci-architecture/README.md#every-terraform-variable) has the full list.

> [!NOTE]
> **Leave `node_client_cidr_allow_list` at its `0.0.0.0/0` default for now.** It is an OCI security-list rule covering the whole public subnet, and your guests' public addresses cross it too, so narrowing it to your own address cuts internet access to every guest rather than just to the node. [Harden it before production](#harden-it-before-production) locks the node down in the layer that can tell the two apart.

## 5. Deploy

```bash
./validate-topology.sh \
    --topology vm-single \
    --channel dev \
    --credential-hook ~/.spinifex/oci-credential-hook.sh \
    --keep
```

One command builds the infrastructure, installs Spinifex, forms the cluster, configures OCI public addressing, verifies it, then launches real guests on public addresses and tears those guests down again.

| Argument | What to pass |
| --- | --- |
| `--topology` | `vm-single` for one VM, `vm-multi` for three, `bm` for one bare-metal host. Required, with no default, so a command cannot be aimed at the wrong one by omission |
| `--channel` | **`dev` until the next release ships**, because OCI support is not in the current stable release yet. It resolves to the newest pre-release at the moment you run it. `latest` is the default and becomes the right answer once a release carries OCI support |
| `--credential-hook` | Your script from step 2. With an instance principal, replace it with `--instance-principal` |
| `--keep` | **This is what makes it a deployment rather than a test.** Without it the driver destroys everything at the end, which is right for CI and wrong here |

Add `--skip-workload` to stop once the cluster is verified, without launching the validation guests. Add `--dry-run` to print the plan and change nothing.

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
[validate-vm-single] configuring the IMDS remap and the external pool
[validate-vm-single] checking the allocator came up
[validate-vm-single] 150.230.13.131 allocator ready
[validate-vm-single] cluster membership
[validate-vm-single] --keep: leaving the infrastructure up, no verdict recorded
```

**`allocator ready` is the line to look for.** Every line above it also prints on a node that cannot allocate a public address, and that failure stays silent until a guest asks for one.

Then prove it the way a customer would. On the node, use the `[spinifex]` profile that `spx admin init` wrote into `~/.aws/credentials`:

```bash
export AWS_PROFILE=spinifex

aws ec2 allocate-address
aws ec2 run-instances --image-id <ami> --instance-type t3.micro --key-name <key> \
    --subnet-id <subnet> --associate-public-ip-address
aws ec2 describe-instances --query \
    'Reservations[].Instances[].[InstanceId,PublicIpAddress,State.Name]' --output text
```

Then reach the guest **from your workstation, not from the node**:

```bash
ping -c 3 <public-ip>
ssh -i <key> ubuntu@<public-ip> hostname
```

> [!IMPORTANT]
> **Probe a public address from outside the VCN.** A node is a poor vantage point for its own network's public addresses, so a timeout measured from a node says nothing about the guest. Private addresses are a separate question, and those do work from inside.

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
