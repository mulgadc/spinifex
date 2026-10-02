---
title: "Spinifex on Oracle Cloud"
seoTitle: "Run Spinifex on Oracle Cloud Infrastructure — Spinifex Docs"
description: "Deploy Spinifex on OCI end to end with Terraform, single node or three, and give guests real public addresses through OCI's own API."
category: "Install"
tags:
  - install
  - oci
  - oracle cloud
  - terraform
  - cluster
resources:
  - title: "Multi-Node Install"
    url: "/docs/install-multi-node"
  - title: "VPC Networking"
    url: "/docs/vpc-networking"
  - title: "Host Firewall"
    url: "/docs/host-firewall"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
---

# Running Spinifex on Oracle Cloud Infrastructure

> Run the AWS surface — EC2, EBS, S3, VPC — inside your own OCI tenancy, with guests that get real, publicly reachable addresses through OCI's API.

## Table of Contents

- [Overview](#overview)
- [Why run Spinifex on OCI](#why-run-spinifex-on-oci)
- [How it fits together](#how-it-fits-together)
- [Prerequisites](#prerequisites)
- [Deploying](#deploying)
  - [Step 1. Set your Terraform inputs](#step-1-set-your-terraform-inputs)
  - [Step 2. Deploy](#step-2-deploy)
  - [Step 3. Set up your cluster](#step-3-set-up-your-cluster)
  - [Step 4. Run your existing Terraform against the cluster](#step-4-run-your-existing-terraform-against-the-cluster)
  - [Step 5. Oracle Linux as a guest image](#step-5-oracle-linux-as-a-guest-image)
- [Verify end to end](#verify-end-to-end)
- [Troubleshooting](#troubleshooting)
- [Deploying without Terraform](#deploying-without-terraform)

---

## Overview

Spinifex is an open-source infrastructure platform that brings core AWS services to bare-metal, edge and on-prem environments. A node runs the Spinifex daemon and CLI, QEMU/KVM for guests, OVN/Open vSwitch for VPC networking, [Predastore](https://github.com/mulgadc/predastore) for S3-compatible object storage and [Viperblock](https://github.com/mulgadc/viperblock) for EBS-compatible block storage — and serves them all behind a SigV4 endpoint that the ordinary AWS SDKs and CLI talk to unmodified.

This guide runs that same stack on Oracle Cloud Infrastructure. Everything in [Single-Node Install](/docs/install) and [Multi-Node Install](/docs/install-multi-node) still applies: one node is a working install, three is the minimum for a cluster that can lose a node, and formation, storage and OVN behave exactly as they do on hardware you own. What changes is the layer underneath — an OCI VCN is not an Ethernet segment, so the external datapath is wired differently, and public addresses come from OCI's API rather than from a range you write in a config file.

**Read this guide end to end and you have a working cluster.** [Deploying](#deploying) is one sequence of eight steps, from an empty compartment to a node that can launch an instance with a publicly reachable address, and then to the Oracle Linux guest image to put on it. It builds the infrastructure with Terraform, then installs and forms Spinifex the same way [Single-Node Install](/docs/install) and [Multi-Node Install](/docs/install-multi-node) do — those steps are inlined here, so you do not need to read three documents at once. The only choice to make is one node or three, and it is one line in Step 1.

Every command and every output below was run against a live OCI tenancy, on both paths.

## Why run Spinifex on OCI

**Lower cost for the same shape of workload.** OCI's compute and block storage are priced well below the large US clouds for equivalent OCPU and IOPS, and Spinifex turns one large instance into a whole EC2/EBS/S3 surface — so a tenant's twenty small instances are twenty QEMU guests on hardware you are billed for once, not twenty billable cloud instances.

**Egress stops being the line item that decides the architecture.** Oracle publishes a large monthly outbound-transfer allowance per tenancy and a per-GB rate beyond it that is roughly an order of magnitude below the majors — check the current pricing page rather than a number in a document, but the gap is the reason this integration exists. Traffic _between_ guests never leaves the host or, on a cluster, never leaves the VCN: it is Geneve over the private plane, which is not billed as internet egress at all.

**Portability — the workload stops being AWS-shaped and starts being yours.** The API your tenants code against is Spinifex's, so the same Terraform, the same SDK calls and the same AMIs run on OCI today, on bare metal in your own rack tomorrow, and at an edge site after that. Moving off AWS becomes a deployment decision rather than a rewrite, and moving _again_ costs nothing extra.

**You keep the parts of OCI that are worth keeping.** Bare-metal shapes, fault domains, block volumes with a chosen performance tier, and real public addresses out of OCI's own pool. Spinifex does not hide the cloud underneath it — it registers each guest's address with OCI so that guest is genuinely reachable on the internet, not behind a shared NAT.

### Sizing

**Bare metal is the recommendation.** `BM.Standard.E2.64` or similar gives you the hardware directly — no nested virtualisation, full NIC performance, and no hypervisor between your guests and the wire. If bare metal is out of scope for your deployment using a single `VM.Standard.E6.Flex` instance is recommended for testing/development purposes, or 3 `VM.Standard.E6.Flex` nodes for production.

|                 | Minimum               | Recommended                                                       |
| --------------- | --------------------- | ----------------------------------------------------------------- |
| **Shape**       | `VM.Standard.E6.Flex` | Bare metal, e.g. `BM.Standard.E2.64`                              |
| **OCPU**        | 8 (16 vCPU)           | 32+                                                               |
| **RAM**         | 32 GB                 | 128 GB+                                                           |
| **Data volume** | 256 GiB block volume  | 1 TB+ NVMe-backed                                                 |
| **VNICs**       | 2                     | 2                                                                 |
| **Nodes**       | 1                     | 3 — see [Cluster sizing](/docs/install-multi-node#cluster-sizing) |

---

## How it fits together

### Single node — the whole AWS surface inside one OCI instance

One bare-metal instance runs the control plane and every tenant guest. Customers reach the AWS APIs on the node's own address; their instances reach the internet through addresses registered on the node's second VNIC.

<p align="center">
  <img src="../../.github/assets/diagrams/oci-single-node.svg" alt="Single node on OCI — the instance inside the VCN public subnet, both VNICs, br-wan, the routed-NAT transit veth, the control plane, tenant VPCs and the iSCSI data volume" width="900">
</p>

Two things in that picture are the whole integration. The guest's private address (`10.0.1.4`) never leaves the host — OVN and the host route both hold it. The address OCI delivers to is a **secondary private IP registered on VNIC 1**, appearing on `br-wan`, and the customer's public address (`150.230.13.131`) is a reserved public IP that OCI 1:1-NATs onto it upstream. `describe-instances` reports the public one; every datapath object holds the private one, which is why the two never appear together on the host.

The third is storage. Every guest disk, every S3 object and the JetStream state all live under `/var/lib/spinifex`, which is an **OCI block volume attached over iSCSI**, not the boot disk. Size it for the guests you intend to run; a boot volume that quietly ends up carrying the stack is the most common way an install on OCI fails later rather than immediately.

### Three nodes — the same surface, distributed

Each node registers the addresses for **its own** guests on **its own** second VNIC, and the allocator runs per node, so nothing has to know another node's OCIDs. When a guest moves — a stop/start that lands elsewhere, or a host failure — the node it lands on claims the address's private half from the old VNIC through a single OCI call. The OCID and the public half are untouched, so the customer's address never changes.

<p align="center">
  <img src="../../.github/assets/diagrams/oci-three-node.svg" alt="Three nodes on OCI — three instances inside the VCN public subnet, one per fault domain, each with its own VNICs, addresses and iSCSI volume, joined east-west by Geneve, NATS, predastore and the OVN raft" width="900">
</p>

The overlay, the object shards and the OVN databases all cross **VNIC 0**, inside the VCN. Size that plane, not the external one: guest-to-guest traffic between nodes is Geneve over the VCN, and it is the link every distributed layer shares.

> [!NOTE]
> **The Spinifex region is your own naming and has nothing to do with the OCI region.** `ap-southeast-2` in `spinifex.toml` beside `ap-sydney-1` in OCI is correct and expected. Do not read one as evidence about the other.

---

## Prerequisites

Both paths need the same three things: an OCI compartment you can write to, an API key for Spinifex to allocate addresses with, and enough quota to hand out the addresses you plan to use.

### Credentials — an instance principal, or an API key

The allocator needs OCI credentials at runtime, and there are two ways to give it them. **A node with neither forms, passes every health check, and then refuses every launch that wants a public address** with `InsufficientAddressCapacity`, naming no cause outside its own journal.

**An instance principal is the better one, and it is now a one-time setup step.** The instance certificate is served at `/opc/v2/identity/cert.pem` and the Go SDK authenticates as the instance with no key material on disk — but it must also be _authorised_, by a dynamic group and a policy. On the reference tenancy it authenticated without being authorised for anything, which is the failure above. Those two resources live at the tenancy root, so creating them needs a tenancy admin once; every deployment and rebuild afterwards needs only a compartment-scoped user:

```bash
cd scripts/terraform/oci-spx
./setup-identity.sh --dry-run     # always first
./setup-identity.sh               # needs a tenancy-admin OCI profile
```

Then set `instance_principal = "adopt"` in your tfvars. The dynamic group matches on `instance.compartment.id`, so it covers every node you ever build in that compartment and nothing needs updating when nodes are replaced.

**An API key is the fallback**, and the default, because it needs nothing from a tenancy admin — which makes it the only option when the compartment belongs to someone else's tenancy. The cost is key material on disk, with a rotation obligation, on every node:

```bash
# On your workstation, not the node.
openssl genrsa -out ~/.oci/oci_api_key.pem 2048
chmod 600 ~/.oci/oci_api_key.pem
openssl rsa -pubout -in ~/.oci/oci_api_key.pem -out ~/.oci/oci_api_key_public.pem
openssl rsa -pubout -outform DER -in ~/.oci/oci_api_key.pem \
  | openssl md5 -c        # this is the fingerprint
```

Upload the public key to the user under **Identity → Users → API Keys**, and record the user OCID, the fingerprint and the tenancy OCID.

Each node then needs **two** files, and a node with only the first is the failure at the top of this section — it forms, looks healthy, and cannot allocate:

| Path                                | Mode                 | Contents                                                                 |
| ----------------------------------- | -------------------- | ------------------------------------------------------------------------ |
| `/etc/spinifex/oci/oci_api_key.pem` | `0640 root:spinifex` | The private key                                                          |
| `/etc/spinifex/oci/config`          | `0640 root:spinifex` | An ordinary OCI SDK config, profile `[spinifex]`, naming the key by path |

Nothing creates the second one for you, and its profile name must be `spinifex` to match `oci_config_profile` in the pool:

```ini
[spinifex]
user=ocid1.user.oc1..aaaa...
fingerprint=12:34:56:78:90:ab:cd:ef:12:34:56:78:90:ab:cd:ef
tenancy=ocid1.tenancy.oc1..aaaa...
region=ap-sydney-1
key_file=/etc/spinifex/oci/oci_api_key.pem
```

**Put that in a credential hook and the one-command path installs it on every node.** The hook is an executable of yours, run as `hook <ssh-key> <host>...` after formation and before the pool is configured, which is the only window where a node has `/etc/spinifex` but has not started the allocator. It is yours rather than ours deliberately: a credential belongs to whoever owns it, and one rendered into user-data or Terraform state is readable from instance metadata for the life of the instance — on a host that runs other people's guests.

### The IAM policy

Spinifex calls exactly nine operations. Grant no more than these:

| Operation                                  | Why                                                                              |
| ------------------------------------------ | -------------------------------------------------------------------------------- |
| `CreatePrivateIp`                          | Register the secondary private IP the datapath carries                           |
| `DeletePrivateIp`                          | Release it                                                                       |
| `GetPrivateIp`                             | Poll for `AVAILABLE` after an asynchronous assign                                |
| `ListPrivateIps`                           | The startup reconcile, and the affinity pass that moves an address between nodes |
| `UpdatePrivateIp`                          | Reassign a private IP to this node's VNIC when a guest moves here                |
| `CreatePublicIp`                           | Create the RESERVED public IP                                                    |
| `DeletePublicIp`                           | Release it                                                                       |
| `GetPublicIp` / `GetPublicIpByPrivateIpId` | Poll lifecycle state, and reconcile a private IP back to its public half         |
| `UpdatePublicIp`                           | Attach and detach the public IP from a private IP                                |

The matching policy, scoped to the one compartment Spinifex allocates in:

```text
Allow group SpinifexOperators to manage private-ips in compartment <compartment-name>
Allow group SpinifexOperators to manage public-ips  in compartment <compartment-name>
Allow group SpinifexOperators to read   vnics       in compartment <compartment-name>
Allow group SpinifexOperators to read   subnets     in compartment <compartment-name>
Allow group SpinifexOperators to read   vcns        in compartment <compartment-name>
```

**Scope it to a compartment, not the tenancy.** `manage public-ips` at tenancy level lets a compromised node consume the whole regional reserved-public-IP quota.

**Do not grant `manage instances` or `manage vnics`.** Spinifex never creates, attaches or deletes a VNIC — it only adds addresses to a VNIC that already exists. A grant that allows VNIC lifecycle is strictly more than the integration can use.

### Quotas to check before you size a deployment

Two limits bound how many public addresses a deployment can hand out, and they do not scale the same way:

| Quota                              | Default | Scope                     | Notes                                                                                          |
| ---------------------------------- | ------- | ------------------------- | ---------------------------------------------------------------------------------------------- |
| **Reserved public IPs**            | 50      | Per region, whole tenancy | Shared by every node, so it does **not** grow with node count. This is the wall you hit first. |
| **Secondary private IPs per VNIC** | 64      | Per VNIC, so per node     | Not raisable. Three nodes gives you 192 — this is the limit that does scale.                   |

A raise on the first is a support request, not a setting: Console → Governance & Administration → Limits, Quotas and Usage → "Request a service limit increase". Cite the exact limit name from `oci limits definition list`, not a name from documentation.

```bash
oci limits definition list --service-name vcn --compartment-id <tenancy-ocid>
oci limits value list       --service-name vcn --compartment-id <tenancy-ocid>
oci limits resource-availability get --service-name vcn \
    --limit-name <name-from-above> --compartment-id <tenancy-ocid>
```

**These calls need a grant the node does not have, and must not be given.** Limits and usage are tenancy-level, which cannot be compartment-scoped the way the policy above insists the node's is. Create a **second, read-only principal** for an operator or a reporting job and keep it off every node:

```text
Allow group SpinifexReporting to read limits        in tenancy
Allow group SpinifexReporting to read usage-reports in tenancy
```

The second verb is what Cost Analysis and `oci usage-api usage-summary request-summarized-usages` read. Adding either to `SpinifexOperators` would hand every node tenancy-wide read, which is exactly what the node policy refuses.

### The `oci` CLI

**The `oci` CLI is not on the Ubuntu image**, and `apt` has no `python3-oci-cli` package. Install it into a virtualenv on your workstation, and on the node if you want to inspect allocations there:

```bash
sudo apt-get update && sudo apt-get install -y python3-venv
python3 -m venv ~/.oci-cli-venv
~/.oci-cli-venv/bin/pip install --upgrade pip oci-cli
sudo ln -sf ~/.oci-cli-venv/bin/oci /usr/local/bin/oci
oci --version
```

**What the CLI is and is not for.** Spinifex's daemon uses the OCI **Go SDK** for every allocation — the CLI is not on that path. You need it for bootstrap and for operational inspection. Do not build automation that shells out to it during an allocation; allocations run inside a request budget that a forked Python process will not respect.

---

## Deploying

Two steps: fill in one file, run one command. Everything else on this page is reference.

|                                                 |                                                                                                                                       |
| ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| **[Step 1](#step-1-set-your-terraform-inputs)** | Write `terraform.auto.tfvars` — your compartment, your shape, how many nodes                                                          |
| **[Step 2](#step-2-deploy)**                    | Run one command. It builds the infrastructure, installs Spinifex, forms the cluster, configures OCI public addressing and verifies it |

Steps 3 to 5 then cover using the cluster you just built. [Deploying without Terraform](#deploying-without-terraform) is for a tenancy you cannot run Terraform against.

**The only decision to make first is how many nodes.** It is a single line in Step 1:

|                                | Single node                                  | Three nodes                                                                                      |
| ------------------------------ | -------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| **`node_count`**               | `1`                                          | `3`                                                                                              |
| **Survives losing a node**     | No                                           | Yes — NATS keeps quorum, predastore RS(2,1) keeps the data readable, the OVN raft keeps a leader |
| **Public addresses available** | 64 — the per-VNIC secondary private IP limit | 192, three VNICs' worth, still bounded by the regional reserved-public-IP quota                  |
| **Fault domains**              | One                                          | Three, one per node, chosen by Terraform                                                         |
| **Use it for**                 | Evaluation, a lab, an edge site with one box | Anything you would be unhappy to lose                                                            |

Two is not an option worth taking: it doubles the cost of a single node and gives you a cluster that cannot form a quorum. See [Cluster sizing](/docs/install-multi-node#cluster-sizing).

The configuration lives in [`scripts/terraform/oci-spx`](../../scripts/terraform/oci-spx/README.md), which has the resource-by-resource architecture.

### What Terraform builds, per node

| Resource                     | Detail                                                                                                                                             |
| ---------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `oci_core_instance`          | One per `node_count`, round-robined across **fault domains** so three nodes land on three sets of racks rather than wherever placement puts them   |
| Primary VNIC                 | Public IP, `hostname_label`, the host plane — SSH, the advertise address, the Geneve endpoint                                                      |
| `oci_core_vnic_attachment`   | A **second VNIC** in the same subnet. Every Spinifex external address becomes a secondary private IP here at runtime                               |
| `oci_core_volume`            | A block volume per node, `data_volume_size_in_gbs` at `data_volume_vpus_per_gb`                                                                    |
| `oci_core_volume_attachment` | **`attachment_type = "iscsi"`**, with the Oracle Cloud Agent's Block Volume Management plugin enabled so the node logs the iSCSI session in itself |
| cloud-init                   | Partitions, formats, mounts and `fstab`s the volume; builds `br-wan` over the second VNIC; opens the service ports                                 |

**The data volume is iSCSI, and that is the detail that bites.** OCI presents the volume as an iSCSI target reachable at `169.254.2.2:3260` — attaching it in the API does not put a block device on the host. `is_agent_auto_iscsi_login_enabled` makes the Oracle Cloud Agent do the login, and because it does that _asynchronously_, the device is not there when cloud-init first runs. The mount script waits up to ten minutes for it, formats it **only if `blkid` reports no filesystem** (so a re-run cannot erase a populated volume), and writes an fstab entry by UUID:

```text
UUID=<uuid>  /var/lib/spinifex  ext4  defaults,_netdev,nofail  0 2
```

`_netdev` is what orders the mount after the network, and therefore after `iscsid` has a session. `nofail` is what keeps a missing volume from wedging the boot. Mounting `/var/lib/spinifex` _itself_ rather than somewhere else and symlinking is deliberate: viperblock, predastore and JetStream all land on the volume with no configuration pointing anywhere unusual.

---

## Step 1. Set your Terraform inputs

**This step is required either way** — it is the one hand-edited file, and the one-command path above reads it too. Steps 2 to 5 are what that command automates.

Prerequisites: Terraform, Python 3, OCI credentials in `~/.oci/config` or the environment, and an SSH key. The Python helper is standard library only, so there is nothing to install.

```bash
cd scripts/terraform/oci-spx
cat > terraform.auto.tfvars <<'EOF'
compartment_ocid            = "ocid1.compartment.oc1..aaaa..."
deployment_name             = "spinifex"

# Shape and size. Bare metal is preferred; a flex VM is what the quota allows.
compute_shape               = "VM.Standard.E6.Flex"
compute_ocpus               = 8          # OCI counts an OCPU as a full core
compute_memory_in_gbs       = 32

# 1 for a single node, 3 for a cluster. This is the only line that chooses.
node_count                  = 3

# The block volume behind /var/lib/spinifex, attached over iSCSI.
data_volume_size_in_gbs     = 256
data_volume_vpus_per_gb     = 120        # 120 = Ultra High Performance

node_client_cidr_allow_list = ["203.0.113.10/32"]
EOF
```

The variables worth knowing, all of which have defaults that build a working single node:

| Variable                      | Default               | What it decides                                                                                                                                                                                                                                      |
| ----------------------------- | --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `compartment_ocid`            | —                     | **Required.** An OCID, not a name: this deploys into a compartment that already exists, because creating one needs tenancy-root rights the API user must not have                                                                                    |
| `deployment_name`             | `spinifex`            | Prefixes every resource and becomes the VCN `dns_label`, so 1–15 lowercase alphanumerics starting with a letter                                                                                                                                      |
| `node_count`                  | `1`                   | Nodes, 1–25. `1` for a single node, `3` for a cluster. Each gets its own volume, its own second VNIC and its own fault domain                                                                                                                        |
| `compute_shape`               | `VM.Standard.E6.Flex` | **`BM.*` for bare metal, `VM.*.Flex` for a VM.** A fixed bare-metal shape carries its own size, so `shape_config` is emitted only when the name contains `Flex` — set a BM shape and the two sizing variables below are ignored rather than rejected |
| `compute_ocpus`               | `8`                   | OCPUs on a flex shape. Validated at ≥ 8 (16 vCPU), the minimum recommended for a node                                                                                                                                                                |
| `compute_memory_in_gbs`       | `32`                  | GiB on a flex shape. Validated at ≥ 32                                                                                                                                                                                                               |
| `data_volume_size_in_gbs`     | `256`                 | Size of each node's data volume. This is where guest disks, S3 objects and JetStream live — size it for the guests, not for the control plane                                                                                                        |
| `data_volume_vpus_per_gb`     | `120`                 | Performance tier, 0–120 VPUs/GB. 120 is Ultra High Performance; 10 is Balanced. Higher is more IOPS and more cost                                                                                                                                    |
| `data_mountpoint`             | `/var/lib/spinifex`   | Where cloud-init mounts it. Change only if you know why                                                                                                                                                                                              |
| `vcn_cidr`                    | `10.200.0.0/22`       | VCN address space. A `/22` leaves room for the secondary private IPs Spinifex registers per node                                                                                                                                                     |
| `public_subnet_cidr`          | `10.200.0.0/23`       | The subnet every node sits in, **on both VNICs**. Nodes serve the UI, the S3 gate and the AWS gateway directly, so they cannot live in a private subnet                                                                                              |
| `node_client_cidr_allow_list` | `["0.0.0.0/0"]`       | Who may reach SSH and the service ports from the internet. **Change this**                                                                                                                                                                           |
| `node_service_ports`          | `[3000, 8443, 9999]`  | Console, predastore gate, AWS gateway. SSH is opened separately                                                                                                                                                                                      |
| `ubuntu_version`              | `26.04`               | Canonical platform image version. The newest image for the shape and region is resolved at plan time rather than pinned to a regional OCID                                                                                                           |
| `wan_bridge_name`             | `br-wan`              | The Linux bridge built over the second VNIC                                                                                                                                                                                                          |
| `wan_bridge_mtu`              | `9000`                | The OCI VCN carries 9000                                                                                                                                                                                                                             |

> [!WARNING]
> **`node_client_cidr_allow_list` defaults to `0.0.0.0/0`.** Nodes sit in a public subnet with public addresses because they serve the UI, the S3 gate and the AWS gateway directly — there is no bastion. Restrict this before any real use.

## Step 2. Deploy

One command builds the infrastructure, installs Spinifex, forms the cluster, configures OCI public addressing and verifies the result. Run it from the repository, in the same directory as the `terraform.auto.tfvars` you just wrote:

```bash
cd scripts/terraform/oci-spx

./validate-topology.sh \
    --topology vm-single \
    --version v1.21.0-70-g1743b602-dev-1-g66bc1172-dev \
    --credential-hook ~/.spinifex/oci-credential-hook.sh \
    --keep
```

It takes about ten minutes for a single node and prints each stage as it finishes. The four arguments:

| Argument            | What to pass                                                                                                                                                                             |
| ------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--topology`        | `vm-single` for one VM, `vm-multi` for a three-node cluster, `bm` for one bare-metal host. Required, with no default, so a command cannot be aimed at the wrong one by omission.         |
| `--version`         | The release to install. **Use the tag above until the next release ships**, because OCI support is not in the current stable release yet.                                                |
| `--credential-hook` | Only when nodes authenticate with an API key — see [Credentials](#credentials--an-instance-principal-or-an-api-key). With an instance principal, replace it with `--instance-principal`. |
| `--keep`            | Leaves the deployment running. Without it the driver destroys everything at the end, which is what CI wants and not what you want.                                                       |

> [!TIP]
> Prefer `--version <tag>` over `--channel dev`. Both install a prerelease, but `--channel dev` resolves through a GitHub API call that is rate limited per source address, and when it trips it returns a 404 that looks exactly like a missing release. A tag is a plain redirect and has no such failure mode.

Add `--skip-workload` to stop after verifying the cluster. Without it, the driver also launches real guests on public addresses and tears them down again, which is the only check that proves end-to-end connectivity rather than inferring it.

### If a stage fails

The driver stops at the first failure, names the log, and leaves the rest alone. Logs are all under `.validate-<topology>/`:

| Stage                                           | Log                                      |
| ----------------------------------------------- | ---------------------------------------- |
| Building the infrastructure                     | `apply.log`                              |
| Installing Spinifex                             | `install-<host>.log`                     |
| Forming the cluster                             | `form.log`                               |
| Installing the credential, configuring the pool | `credential-hook.log`, `pool-<host>.log` |
| Verifying public addressing                     | `allocator-<host>.log`                   |

Add `--keep-on-fail` to leave a failed deployment running so you can log in and look. **It keeps billing** until you run `--destroy-only`.

The last log is the one worth knowing about, because public addressing is the part with the most moving pieces. If the allocator never comes up, the driver captures the daemon journal, the pool as configured, the bridge and OCI MAC addresses, and whether each credential file exists and is readable — never the credentials themselves — before anything is torn down. [Troubleshooting](#troubleshooting) reads those.

To remove everything:

```bash
./validate-topology.sh --topology vm-single --destroy-only
```

**`--keep` is what makes this a deployment rather than a test.** Without it the driver destroys everything at the end, which is correct for CI and not what you want here. `--skip-workload` skips launching the throwaway validation guests; drop it to have the driver prove the deployment with real instances on public addresses before handing it over.

## Step 3. Set up your cluster

The cluster is running, but it holds nothing yet — no machine images, no networks, no instances.

Continue to [Setting Up Your Cluster](/docs/setting-up-your-cluster) to import an AMI, create an SSH key pair, create a VPC with a public subnet, and launch your first instance. It ends by arming the [host firewall](/docs/host-firewall), which is also where you re-arm it if you turned it off to form the cluster.

---

## Step 4. Run your existing Terraform against the cluster

This is the part worth pausing on. Everything above builds infrastructure **on** OCI using the OCI provider. Everything from here uses the **AWS** provider — unmodified, from the OpenTofu or Terraform registry — pointed at your own cluster. The same `aws_vpc`, `aws_instance`, `aws_db_instance`, `aws_ecs_service` and `aws_eks_cluster` resources a team already has in git apply against Spinifex on an OCI tenancy, with no rewriting and no OCI-specific module.

That is the claim, so it is measured rather than asserted. What follows is a run of the workbooks we ship against a three-node Spinifex cluster on OCI VMs.

### Pointing the provider at your cluster

Three things differ from a provider block aimed at AWS, and only three:

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

Credentials come from the `[spinifex]` profile that `spx admin init` writes into `~/.aws/credentials` on node 1.

> **Use the node's private address, not its OCI reserved public IP.** The node certificate carries no SAN for the public address, because that address is never on the wire — OCI NATs it to a private one. An `aws` or `terraform` call to `https://<public IP>:9999` fails TLS verification with _hostname doesn't match_. Until that is fixed, run Terraform from a node, or from anything else inside the VCN.

### What we tested, and what happened

Measured on 2026-09-26 against a three-node cluster of `VM.Standard.E6.Flex` instances in `ap-sydney-1`, running `spinifex v1.20.0-113`. Each workbook is a full `apply`, a functional assertion against the thing it built, and a `destroy`.

| Workbook                 | What it exercises                                                                               | Result            |
| ------------------------ | ----------------------------------------------------------------------------------------------- | ----------------- |
| `nginx-webserver`        | VPC, subnet, IGW, security group, key pair, EC2 instance, public IP; HTTP 200 from the internet | **Passed** (86s)  |
| `bastion-private-subnet` | Public and private subnets, NAT egress, a bastion, SSH hop to a private host                    | **Passed** (117s) |
| `rds-quickstart`         | DB subnet group, parameter group, `aws_db_instance` (PostgreSQL), client instance, connection   | **Passed** (296s) |
| `ecs-quickstart`         | ECS cluster, task definition, service, container instances, ALB with a healthy target           | **Passed** (166s) |
| `eks-quickstart`         | EKS control plane, managed node group, `kubectl` against the cluster, nodes `Ready`             | **Passed** (259s) |
| `nginx-alb`              | Application Load Balancer across two subnets, two backends, health checks                       | **Passed** (123s) |
| `s3-webapp`              | S3 bucket, IAM role and instance profile, IMDS-fetched credentials, upload from the guest       | **Passed**        |
| `demo-app`               | Container image for the EKS workbooks, pushed to ECR                                            | **Passed**        |
| `eks-https-ingress`      | Private-subnet workers behind a NAT gateway, LBC addon, ACM certificate, HTTPS Ingress          | **Passed** (252s) |
| `eks-gitops-argocd`      | Private-subnet workers, Argo CD addon, EBS-CSI PersistentVolume, GitOps sync                    | **Passed** (225s) |

---

## Step 5. Oracle Linux as a guest image

Spinifex ships four Oracle Linux entries in its image catalog, so a customer on OCI can run the same distro their Oracle support contract covers. They import exactly like any other AMI — nothing about them is OCI-specific, and they run equally well on bare metal:

| Catalog name         | Release           | Arch   | Kernel |
| -------------------- | ----------------- | ------ | ------ |
| `oracle-10.1-x86_64` | Oracle Linux 10.1 | x86_64 | UEK 8  |
| `oracle-10.1-arm64`  | Oracle Linux 10.1 | arm64  | UEK 8  |
| `oracle-9.8-x86_64`  | Oracle Linux 9.8  | x86_64 | UEK 7  |
| `oracle-9.8-arm64`   | Oracle Linux 9.8  | arm64  | UEK 7  |

```bash
sudo spx admin images import --name oracle-10.1-x86_64 --config /etc/spinifex/spinifex.toml
aws ec2 describe-images --query 'Images[].[Name,State,BootMode]' --output text
```

All four boot **UEFI**, so launch them into a shape that boots UEFI — a BIOS-only instance type will not come up, and the failure looks like a hung boot rather than a rejected image.

### The login user is `cloud-user`, not `opc`

```bash
ssh -i path/to/key.pem cloud-user@<public-ip>
```

**This is the one that catches people.** `opc` is the default user on the images Oracle publishes _into OCI itself_; these are the generic **KVM cloud images** from `yum.oracle.com`, whose cloud-init `default_user` is `cloud-user`. Verified on both releases: `opc`, `oracle` and `ec2-user` are all refused. The Spinifex UI's instance detail page shows the right user per AMI, so read it there rather than guessing from the distro.

---

---

## Verify end to end

```bash
export AWS_PROFILE=spinifex

aws ec2 allocate-address
aws ec2 run-instances --image-id <ami> --instance-type t3.micro --key-name <key> \
    --subnet-id <subnet> --associate-public-ip-address
aws ec2 describe-instances --query \
    'Reservations[].Instances[].[InstanceId,PrivateIpAddress,PublicIpAddress,State.Name]' \
    --output text
```

Then reach the guest on its public address — **from your workstation, not from the node**:

```bash
ping -c 3 <public-ip>
ssh -i <key> ubuntu@<public-ip> hostname
```

> [!IMPORTANT]
> **A VCN does not route its own public addresses back into itself.** AWS reflects them through the internet gateway; OCI answers nothing, so a node asking for a guest's public address gets a timeout however well the datapath works. Probe from outside the VCN or the result says nothing. A node's own private-address reachability (`ssh <private-ip>`) is the separate question, and that one does work from inside.

**Confirm the OCI allocator came up**, which `spx get nodes` cannot tell you. Run this on the node:

```bash
sudo journalctl -u spinifex-vpcd --since -5m | grep -i ocinet
```

```text
"msg":"ocinet resolved the external VNIC from its interface","pool":"oci-public","iface":"br-wan",
  "vnic_id":"ocid1.vnic.oc1.ap-sydney-1.abzxsljrjwlf...kimuq","private_ip":"10.200.1.31","subnet":"10.200.0.0/23"
"msg":"OCI allocator ready","pool":"oci-public","collected":0,"stale_bindings":0,"skipped":1
```

**`resolved the external VNIC` is the line that matters** — it proves the credentials work, the compartment is right, and `br-wan`'s MAC matched a real VNIC. A node missing it will accept `allocate-address` and fail it.

> [!NOTE]
> On a cold cluster start you may instead see `OCI allocator reconcile failed … nats: no responders available for request` on some nodes. That is vpcd racing JetStream's KV at boot; the startup reconcile is skipped and not retried. It is harmless on a new cluster where nothing has been allocated, and it is tracked — restart `spinifex-vpcd` on that node to run it. The `resolved the external VNIC` line above is still the one that decides whether allocation works.

And from inside the guest — note that IMDS is **IMDSv2, so it needs a token**; an untokened request correctly returns 401:

```bash
TOK=$(curl -s -X PUT http://169.254.169.254/latest/api/token \
      -H 'X-aws-ec2-metadata-token-ttl-seconds: 120')
curl -s -H "X-aws-ec2-metadata-token: $TOK" \
     http://169.254.169.254/latest/meta-data/instance-id
traceroute -n 8.8.8.8
```

---

## Troubleshooting

### Addresses and quotas

**Spinifex enforces no limit of its own.** It attempts the allocation and reports what OCI says, so the ceiling you hit is always your tenancy's, never a number baked into the software. A quota is raised with Oracle, not reconfigured here, and there is nothing to restart afterwards.

**`InsufficientAddressCapacity` on `allocate-address` is what exhaustion looks like.** It is the AWS error code for "out of addresses" and Spinifex returns it for either quota, mapped from OCI's own refusal. On a cluster the regional one is the likelier of the two.

**A detached reserved public IP still bills.** `disassociate-address` returns the address to `AVAILABLE` without deleting it, exactly as an unassociated AWS Elastic IP behaves, and it keeps consuming your regional quota as well as your bill. `release-address` is what frees both.

**A stopped instance costs nothing for its auto-assigned address.** Stopping an instance returns its auto-assigned address to the pool and deletes the reserved public IP object behind it, as AWS does; the start that follows creates a new one. An **Elastic IP** is yours and survives the stop untouched — that is the whole distinction between the two, and on OCI it is also the difference between a stopped fleet that bills for addresses and one that does not.

### Datapath

**Instance launches but the public address is unreachable.** In order:

1. `sudo ovn-nbctl lr-nat-list <router>` — is there a `dnat_and_snat` row, and does it hold the _private_ address?
2. `ip route get <private-addr>` — does it route via the transit veth?
3. `sudo iptables -S FORWARD | grep spinifex-eip-ingress` — both directions present?
4. `oci network private-ip list --vnic-id <vnic>` — is the private address actually registered with OCI, **and on this node's VNIC**? If not, nothing else matters.

**Egress works from one address and not another on the same NIC.** OCI enforces the source address per VNIC, so this means the address is not a registered private-IP object on it. It is never a routing problem on the host.

**An address went dark after a guest moved node.** The affinity pass should have claimed it within 15 seconds. Check the new node's daemon journal for `ocinet affinity`, then confirm with step 4 above which VNIC OCI thinks holds the private half.

### A node that was fine and then was not

Terraform's cloud-init does the host jobs on each node — the iSCSI volume, `br-wan` and its source routing, the firewall. When one of them did not take, the symptom arrives much later and rarely looks like a networking or storage problem. This one command answers all of it:

```bash
ssh -i ~/.ssh/oci-spx ubuntu@<node-public-ip> '
  cloud-init status                  # done
  sudo iscsiadm -m session           # a session to 169.254.2.2:3260
  findmnt /var/lib/spinifex          # mounted by UUID, with _netdev
  ip -br addr show br-wan            # UP, second VNIC address as a /32
  ip -br addr show enp1s0            # the bridge MEMBER holds NO address
  ip route show default              # still on the PRIMARY interface
  ls -l /dev/kvm                     # present
'
```

Three answers are worth knowing in advance, because each looks wrong and is not:

- **`br-wan` holds a `/32` and the default route is on the other interface.** That is how two VNICs share one subnet without the second stealing the first one's traffic, and it is what satisfies OCI's per-VNIC source check. `br-wan`'s address appears in `ip rule` instead of in the default route.
- **`br-wan`'s address is not one you chose.** It is the second VNIC's own private IP, assigned by OCI, and not predictable before the apply.
- **`enp1s0` carrying an address is the one real fault here.** The kernel derives an on-link route from it that beats the primary VNIC's, so every packet to another node leaves the wrong VNIC and OCI drops it. The node still forms a cluster and still reports `Ready` — formation uses outbound connections — and it surfaces later as `predastore … the stripe is short one holder` on the first AMI import. `sudo /usr/local/sbin/spinifex-setup-wan-bridge` repairs it in place.

### Host

**`RunInstances` returns `ServerInternal`.** Check the daemon journal for the real error — `journalctl -u spinifex-daemon --since -10m`. An OCI allocation failure surfaces this way.

**The stack ends up on the boot disk and the node fills.** The data volume was mounted without `_netdev` and lost the race with `iscsid`. `findmnt /var/lib/spinifex` is the check; `ls` cannot tell the difference.

### Guests cannot reach instance metadata

**Guests boot but cloud-init hangs on metadata**, or the host stops answering its own. OCI uses `169.254.169.254` for its instance metadata, and so does every AWS-compatible guest — the host and the guests want the same address.

Spinifex resolves this by moving the _host_ side of the per-guest metadata link to a different link-local pair, so guests keep asking `169.254.169.254` exactly as they do on AWS. [Step 2](#step-2-deploy) sets `imds_host_meta_ip` and `imds_host_dns_ip` for you. **Set both or neither** — a half-configured pair is ignored and the node behaves as if the remap were off, which is what this symptom looks like.

Check it after your first guest launches:

```bash
ip route get 169.254.169.254                 # your real NIC, not lo
curl -sH 'Authorization: Bearer Oracle' -o /dev/null -w '%{http_code}\n' \
     http://169.254.169.254/opc/v2/instance/ # 200
ip -br addr show | grep ime-                 # holds 169.254.42.x, not .169.x
```

`ip route get 169.254.169.254` returning `dev lo` is the signature of a missing remap. The same signature can appear after a guest terminates, from a leftover `ime-` endpoint still holding the address; with the remap configured it is harmless, and removing the stale OVS port clears it.

## Deploying without Terraform

**Terraform automates every step of a normal install, and that is the point of it.** The `oci-spx` configuration builds the VCN, the subnets, the gateways, the block volumes and both VNICs per node, then renders the cloud-init that installs Spinifex and sets up OVN — so the sequence below happens without anyone typing it, in the same order every time, on one node or three.

If you need to install by hand — an existing OCI tenancy you cannot run Terraform against, an unsupported shape, or a stage you are debugging — the install itself is not OCI-specific. Follow the standard guides:

- [Installing Spinifex](../install/install/README.md) for a single node.
- [Multi-Node Installation](../install/install-multi-node/README.md) for a cluster, which covers `spx admin init` on the leader and `spx admin join` on the rest.

Four things about OCI are not in those guides, and each has its own section above:

- **The external pool** is `source = "oci"`, which needs the instance principal or API key from [Prerequisites](#credentials--an-instance-principal-or-an-api-key) and the [IAM policy](#the-iam-policy). Without it a node forms, passes every health check, and then refuses every launch that wants a public address.
- **`br-wan` stays a Linux bridge** owned by netplan, linked to OVS `br-ext` by a veth pair. Pass `setup-ovn.sh --wan-bridge=br-wan` and it detects this itself; the NIC never becomes an OVS port, so the VNIC keeps its MAC identity. See [How it fits together](#how-it-fits-together).
- **The block volume must be mounted with `_netdev`**, because it arrives over iSCSI and the mount is otherwise attempted before a session exists.
- **IMDS needs the 169.254 remap** described under [Guests cannot reach instance metadata](#guests-cannot-reach-instance-metadata), since the guest's metadata address collides with OCI's own.
