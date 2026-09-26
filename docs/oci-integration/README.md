---
title: "Spinifex on Oracle Cloud"
seoTitle: "Run Spinifex on Oracle Cloud Infrastructure — Spinifex Docs"
description: "Deploy Spinifex on OCI with Terraform or by hand, single node or three, and give guests real public addresses through OCI's own API."
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
- [What OCI changes](#what-oci-changes)
- [How it fits together](#how-it-fits-together)
- [Prerequisites](#prerequisites)
- [Terraform deployment](#terraform-deployment)
- [Manual deployment](#manual-deployment)
- [The `169.254.169.254` collision](#the-169254169254-collision)
- [Verify end to end](#verify-end-to-end)
- [Oracle Linux as a guest image](#oracle-linux-as-a-guest-image)
- [Troubleshooting](#troubleshooting)
- [Limits in this version](#limits-in-this-version)

---

## Overview

Spinifex is an open-source infrastructure platform that brings core AWS services to bare-metal, edge and on-prem environments. A node runs the Spinifex daemon and CLI, QEMU/KVM for guests, OVN/Open vSwitch for VPC networking, [Predastore](https://github.com/mulgadc/predastore) for S3-compatible object storage and [Viperblock](https://github.com/mulgadc/viperblock) for EBS-compatible block storage — and serves them all behind a SigV4 endpoint that the ordinary AWS SDKs and CLI talk to unmodified.

This guide runs that same stack on Oracle Cloud Infrastructure. Everything in [Single-Node Install](/docs/install) and [Multi-Node Install](/docs/install-multi-node) still applies: one node is a working install, three is the minimum for a cluster that can lose a node, and formation, storage and OVN behave exactly as they do on hardware you own. What changes is the layer underneath — an OCI VCN is not an Ethernet segment, so the external datapath is wired differently, and public addresses come from OCI's API rather than from a range you write in a config file.

Two ways to get there, and both end at the same node:

| | [Terraform deployment](#terraform-deployment) | [Manual deployment](#manual-deployment) |
| --- | --- | --- |
| **Nodes** | One or many, `node_count` | One |
| **Builds** | VCN, subnets, gateways, security lists, instances, both VNICs, block volumes, `br-wan`, iSCSI mount, firewall | You do, by hand |
| **Use it when** | Always, unless you cannot | You already have an instance, or you want to see what Terraform is doing |

Neither installs Spinifex itself. That is the standard install path in both cases, and it is the same one bare metal uses.

## Why run Spinifex on OCI

**Lower cost for the same shape of workload.** OCI's compute and block storage are priced well below the large US clouds for equivalent OCPU and IOPS, and Spinifex turns one large instance into a whole EC2/EBS/S3 surface — so a tenant's twenty small instances are twenty QEMU guests on hardware you are billed for once, not twenty billable cloud instances.

**Egress stops being the line item that decides the architecture.** Oracle publishes a large monthly outbound-transfer allowance per tenancy and a per-GB rate beyond it that is roughly an order of magnitude below the majors — check the current pricing page rather than a number in a document, but the gap is the reason this integration exists. Traffic *between* guests never leaves the host or, on a cluster, never leaves the VCN: it is Geneve over the private plane, which is not billed as internet egress at all.

**Portability — the workload stops being AWS-shaped and starts being yours.** The API your tenants code against is Spinifex's, so the same Terraform, the same SDK calls and the same AMIs run on OCI today, on bare metal in your own rack tomorrow, and at an edge site after that. Moving off AWS becomes a deployment decision rather than a rewrite, and moving *again* costs nothing extra.

**You keep the parts of OCI that are worth keeping.** Bare-metal shapes, fault domains, block volumes with a chosen performance tier, and real public addresses out of OCI's own pool. Spinifex does not hide the cloud underneath it — it registers each guest's address with OCI so that guest is genuinely reachable on the internet, not behind a shared NAT.

## What OCI changes

> [!IMPORTANT]
> **Read this section before you provision anything.** OCI enforces addressing inside its virtual network in a way that makes one of Spinifex's three external modes unusable, and the choice is baked in at `spx admin init`. Getting it wrong produces a cluster that looks entirely healthy and drops every guest packet.

Three properties of an OCI virtual network shape everything below.

**OCI enforces the source IP per VNIC.** A packet leaving the instance with a source address that is not a registered private-IP object on that VNIC is dropped by the hypervisor. Measured, not inferred: from the same NIC with the same MAC, `ping -I <registered> 8.8.8.8` gets 0% loss and `ping -I <unregistered> 8.8.8.8` gets 100%. So an external address pool cannot be a range you write in a config file — **every address has to be registered with OCI before it works**, and registering it is exactly what this integration does.

> Pinging the VCN router (`10.200.0.1`) proves nothing. It answers from any source address at ~0.08 ms because the hypervisor replies locally. Always test egress past the subnet.

**A VNIC accepts one MAC; it does not learn MACs.** Every inbound frame arrives addressed to the VNIC's own MAC, whatever address the packet is for. Spinifex's default external datapath advertises each address by ARPing from a per-VPC router MAC onto a shared L2 segment, which needs a segment that learns MACs. OCI has none. Enforcement sits in the hypervisor on VM shapes and in the SmartNIC on bare metal, so there is no guest-side workaround.

**A public IP is a second object, and it never appears on the wire.** An OCI public address is a RESERVED public IP that OCI 1:1-NATs upstream onto a **secondary private IP** on the VNIC. The instance only ever sees the private half. Spinifex handles this internally — AWS-facing APIs report the public address, the datapath uses the private one — but it explains why `ip addr` on the host never shows a customer's public IP, and why that is correct rather than a fault.

### The consequence: routed mode is mandatory

| External mode | What goes on the uplink | On OCI |
| --- | --- | --- |
| `pool` (`UplinkModePhysical`, `UplinkModeVeth`) | Per-VPC gateway MAC plus a per-rule external MAC, ARPed onto the segment | **Does not work.** Egress is dropped (wrong source MAC); ingress never reaches OVN (the frame is addressed to the VNIC MAC, which no router port owns). |
| `nat` (`UplinkModeRouted`) | Nothing. The host forwards over an RFC 6598 transit veth and sets no external MAC | **Works.** One MAC identity, and registered source addresses. |

Spinifex **refuses** `source = "oci"` on a pool unless the node is in `nat` mode, and the refusal names this reason. See [VPC Networking → External modes](/docs/vpc-networking) for what each mode does off OCI.

### Sizing

**Bare metal is the recommendation.** `BM.Standard.E5.192` or similar gives you the hardware directly — no nested virtualisation, full NIC performance, and no hypervisor between your guests and the wire.

| | Minimum | Recommended |
| --- | --- | --- |
| **Shape** | `VM.Standard.E6.Flex` | Bare metal, e.g. `BM.Standard.E5.192` |
| **OCPU** | 8 (16 vCPU) | 32+ |
| **RAM** | 32 GB | 128 GB+ |
| **Data volume** | 256 GiB block volume | 1 TB+ NVMe-backed |
| **VNICs** | 2 | 2 |
| **Nodes** | 1 | 3 — see [Cluster sizing](/docs/install-multi-node#cluster-sizing) |

**Verify nested virtualisation before anything else on a VM shape.** Oracle's nested-KVM guidance names `VM.Standard3.Flex` (Intel) and `VM.Standard.E5.Flex` (AMD), and states Ampere shapes do not support it. `VM.Standard.E6.Flex` is **not** in that list but was measured working on 2026-09-24 (EPYC 9J45, `/dev/kvm` present, `svm` on all threads, `kvm_amd nested=1`). Treat an unlisted shape as unproven until you have checked:

```bash
# If /dev/kvm is absent, stop — change shape, do not continue.
ls -l /dev/kvm
grep -oE 'vmx|svm' /proc/cpuinfo | sort -u
cat /sys/module/kvm_amd/parameters/nested 2>/dev/null \
  || cat /sys/module/kvm_intel/parameters/nested
```

**OCI has no Debian image.** Ubuntu LTS is the tested path — Ubuntu 26.04 LTS on kernel `7.0.0-1009-oracle` is what this integration was built and proven against. Anything in the tooling that assumes Debian is a known gap, not a supported configuration.

---

## How it fits together

### Single node — the whole AWS surface inside one OCI instance

One bare-metal instance runs the control plane and every tenant guest. Customers reach the AWS APIs on the node's own address; their instances reach the internet through addresses registered on the node's second VNIC.

<p align="center">
  <img src="../../.github/assets/diagrams/oci-single-node.svg" alt="Single node on OCI — VCN edge, both VNICs, br-wan, the routed-NAT transit veth, control plane, tenant VPCs and the iSCSI data volume" width="900">
</p>

Two things in that picture are the whole integration. The guest's private address (`10.0.1.5`) never leaves the host — OVN and the host route both hold it. The address OCI delivers to (`10.200.0.40`) is a **secondary private IP registered on VNIC 1**, and the customer's public address is a reserved public IP that OCI 1:1-NATs onto it upstream. `describe-instances` reports the public one; every datapath object holds the private one.

The third is storage. Every guest disk, every S3 object and the JetStream state all live under `/var/lib/spinifex`, which is an **OCI block volume attached over iSCSI**, not the boot disk. Size it for the guests you intend to run; a boot volume that quietly ends up carrying the stack is the most common way an install on OCI fails later rather than immediately.

### Three nodes — the same surface, distributed

Each node registers the addresses for **its own** guests on **its own** second VNIC, and the allocator runs per node, so nothing has to know another node's OCIDs. When a guest moves — a stop/start that lands elsewhere, or a host failure — the node it lands on claims the address's private half from the old VNIC through a single OCI call. The OCID and the public half are untouched, so the customer's address never changes.

<p align="center">
  <img src="../../.github/assets/diagrams/oci-three-node.svg" alt="Three nodes on OCI — per-node VNICs and addresses, the shared Geneve and storage plane over VNIC 0, and an address following a guest between nodes" width="900">
</p>

The overlay, the object shards and the OVN databases all cross **VNIC 0**, inside the VCN. Size that plane, not the external one: guest-to-guest traffic between nodes is Geneve over the VCN, and it is the link every distributed layer shares.

> [!NOTE]
> **The Spinifex region is your own naming and has nothing to do with the OCI region.** `ap-southeast-2` in `spinifex.toml` beside `ap-sydney-1` in OCI is correct and expected. Do not read one as evidence about the other.

---

## Prerequisites

Both deployment paths need the same three things: an OCI compartment you can write to, an API key for Spinifex to allocate addresses with, and enough quota to hand out the addresses you plan to use.

### Credentials — v1 uses an operator-provisioned API key

**Instance principal is the better design and is not what v1 uses.** The instance certificate is served at `/opc/v2/identity/cert.pem` and the Go SDK can authenticate as the instance with no key material on disk — but it still needs a dynamic group and a policy authorising it, and on the reference tenancy it authenticated without being authorised for anything. **v1 requires an operator-provisioned API key**, deliberately, to keep the setup a single documented path.

```bash
# On your workstation, not the node.
openssl genrsa -out ~/.oci/oci_api_key.pem 2048
chmod 600 ~/.oci/oci_api_key.pem
openssl rsa -pubout -in ~/.oci/oci_api_key.pem -out ~/.oci/oci_api_key_public.pem
openssl rsa -pubout -outform DER -in ~/.oci/oci_api_key.pem \
  | openssl md5 -c        # this is the fingerprint
```

Upload the public key to the user under **Identity → Users → API Keys**, and record the user OCID, the fingerprint and the tenancy OCID.

### The IAM policy

Spinifex calls exactly nine operations. Grant no more than these:

| Operation | Why |
| --- | --- |
| `CreatePrivateIp` | Register the secondary private IP the datapath carries |
| `DeletePrivateIp` | Release it |
| `GetPrivateIp` | Poll for `AVAILABLE` after an asynchronous assign |
| `ListPrivateIps` | The startup reconcile, and the affinity pass that moves an address between nodes |
| `UpdatePrivateIp` | Reassign a private IP to this node's VNIC when a guest moves here |
| `CreatePublicIp` | Create the RESERVED public IP |
| `DeletePublicIp` | Release it |
| `GetPublicIp` / `GetPublicIpByPrivateIpId` | Poll lifecycle state, and reconcile a private IP back to its public half |
| `UpdatePublicIp` | Attach and detach the public IP from a private IP |

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

| Quota | Default | Scope | Notes |
| --- | --- | --- | --- |
| **Reserved public IPs** | 50 | Per region, whole tenancy | Shared by every node, so it does **not** grow with node count. This is the wall you hit first. |
| **Secondary private IPs per VNIC** | 64 | Per VNIC, so per node | Not raisable. Three nodes gives you 192 — this is the limit that does scale. |

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

## Terraform deployment

**This is the preferred path, for one node or many.** It builds the VCN, gateways, subnets and security lists, then each node with its two VNICs, its own block volume and the host prerequisites, through cloud-init. What it does not do is install Spinifex; that is Step 4 below, and it is the same install path prod and bare metal use.

The configuration lives in [`scripts/terraform/oci-spx`](../../scripts/terraform/oci-spx/README.md), which has the resource-by-resource architecture.

### What it builds, per node

| Resource | Detail |
| --- | --- |
| `oci_core_instance` | One per `node_count`, round-robined across **fault domains** so three nodes land on three sets of racks rather than wherever placement puts them |
| Primary VNIC | Public IP, `hostname_label`, the host plane — SSH, the advertise address, the Geneve endpoint |
| `oci_core_vnic_attachment` | A **second VNIC** in the same subnet. Every Spinifex external address becomes a secondary private IP here at runtime |
| `oci_core_volume` | A block volume per node, `data_volume_size_in_gbs` at `data_volume_vpus_per_gb` |
| `oci_core_volume_attachment` | **`attachment_type = "iscsi"`**, with the Oracle Cloud Agent's Block Volume Management plugin enabled so the node logs the iSCSI session in itself |
| cloud-init | Partitions, formats, mounts and `fstab`s the volume; builds `br-wan` over the second VNIC; opens the service ports |

**The data volume is iSCSI, and that is the detail that bites.** OCI presents the volume as an iSCSI target reachable at `169.254.2.2:3260` — attaching it in the API does not put a block device on the host. `is_agent_auto_iscsi_login_enabled` makes the Oracle Cloud Agent do the login, and because it does that *asynchronously*, `/dev/oracleoci/oraclevdb` is not there when cloud-init first runs. The mount script waits up to ten minutes for the device, formats it **only if `blkid` reports no filesystem** (so a re-run cannot erase a populated volume), and writes an fstab entry by UUID:

```text
UUID=<uuid>  /var/lib/spinifex  ext4  defaults,_netdev,nofail  0 2
```

`_netdev` is what orders the mount after the network, and therefore after `iscsid` has a session. `nofail` is what keeps a missing volume from wedging the boot. Mounting `/var/lib/spinifex` *itself* rather than somewhere else and symlinking is deliberate: viperblock, predastore and JetStream all land on the volume with no configuration pointing anywhere unusual.

### Step 1. Set your inputs

Prerequisites: Terraform, Python 3, OCI credentials in `~/.oci/config`, and an SSH public key. The Python helper builds and uses the configuration's own `.venv`.

```bash
cd scripts/terraform/oci-spx
cat > terraform.auto.tfvars <<'EOF'
compartment_ocid            = "ocid1.compartment.oc1..aaaa..."
deployment_name             = "spinifex"

# Shape and size. Bare metal is preferred; a flex VM is what the quota allows.
compute_shape               = "VM.Standard.E6.Flex"
compute_ocpus               = 8          # OCI counts an OCPU as a full core
compute_memory_in_gbs       = 32

# One node or a cluster. Each node gets its own volume and its own second VNIC.
node_count                  = 3

# The block volume behind /var/lib/spinifex, attached over iSCSI.
data_volume_size_in_gbs     = 256
data_volume_vpus_per_gb     = 120        # 120 = Ultra High Performance

node_client_cidr_allow_list = ["203.0.113.10/32"]
EOF
```

The variables worth knowing, all of which have defaults that build a working single node:

| Variable | Default | What it decides |
| --- | --- | --- |
| `compartment_ocid` | — | **Required.** An OCID, not a name: this deploys into a compartment that already exists, because creating one needs tenancy-root rights the API user must not have |
| `deployment_name` | `spinifex` | Prefixes every resource and becomes the VCN `dns_label`, so 1–15 lowercase alphanumerics starting with a letter |
| `node_count` | `1` | Nodes, 1–25. `1` for a single node, `3` for a cluster. Each gets its own volume, its own second VNIC and its own fault domain |
| `compute_shape` | `VM.Standard.E6.Flex` | **`BM.*` for bare metal, `VM.*.Flex` for a VM.** A fixed bare-metal shape carries its own size, so `shape_config` is emitted only when the name contains `Flex` — set a BM shape and the two sizing variables below are ignored rather than rejected |
| `compute_ocpus` | `8` | OCPUs on a flex shape. Validated at ≥ 8 (16 vCPU), the minimum recommended for a node |
| `compute_memory_in_gbs` | `32` | GiB on a flex shape. Validated at ≥ 32 |
| `data_volume_size_in_gbs` | `256` | Size of each node's data volume. This is where guest disks, S3 objects and JetStream live — size it for the guests, not for the control plane |
| `data_volume_vpus_per_gb` | `120` | Performance tier, 0–120 VPUs/GB. 120 is Ultra High Performance; 10 is Balanced. Higher is more IOPS and more cost |
| `data_mountpoint` | `/var/lib/spinifex` | Where cloud-init mounts it. Change only if you know why |
| `vcn_cidr` | `10.200.0.0/22` | VCN address space. A `/22` leaves room for the secondary private IPs Spinifex registers per node |
| `public_subnet_cidr` | `10.200.0.0/23` | The subnet every node sits in, **on both VNICs**. Nodes serve the UI, the S3 gate and the AWS gateway directly, so they cannot live in a private subnet |
| `node_client_cidr_allow_list` | `["0.0.0.0/0"]` | Who may reach SSH and the service ports from the internet. **Change this** |
| `node_service_ports` | `[3000, 8443, 9999]` | Console, predastore gate, AWS gateway. SSH is opened separately |
| `ubuntu_version` | `26.04` | Canonical platform image version. The newest image for the shape and region is resolved at plan time rather than pinned to a regional OCID |
| `wan_bridge_name` | `br-wan` | The Linux bridge built over the second VNIC |
| `wan_bridge_mtu` | `9000` | The OCI VCN carries 9000 |

> [!WARNING]
> **`node_client_cidr_allow_list` defaults to `0.0.0.0/0`.** Nodes sit in a public subnet with public addresses because they serve the UI, the S3 gate and the AWS gateway directly — there is no bastion. Restrict this before any real use.

> [!NOTE]
> **Bare metal changes nothing else in this document.** Set `compute_shape = "BM.Standard.E5.192"` and the same config builds it: the two flex sizing variables stop applying, the shape's own CPU and memory take over, and every other step — the volume, the second VNIC, `br-wan`, the install — is identical. Nested virtualisation stops being a question, because there is no nesting.

### Step 2. Plan, review, apply

```bash
KEY=~/.ssh/oci-spx.pub
python3 scripts/oci_env.py --ssh-public-key-path "$KEY" -- terraform init
python3 scripts/oci_env.py --ssh-public-key-path "$KEY" -- terraform plan -out=oci-spx.tfplan
```

Read the saved plan, then apply **that exact plan** rather than re-planning:

```bash
python3 scripts/oci_env.py --ssh-public-key-path "$KEY" -- terraform apply oci-spx.tfplan
terraform output
```

> [!WARNING]
> Never commit the state file, a plan file, or key material. `.gitignore` covers `terraform.tfstate*`, `*.tfplan`, `*.auto.tfvars`, `terraform.tfvars` and `*.pem`.

### Step 3. Confirm what cloud-init did

Terraform's cloud-init does the three host jobs that would otherwise be manual. Check each on every node before installing:

```bash
ssh -i ~/.ssh/oci-spx ubuntu@<node-ip> '
  sudo iscsiadm -m session           # a session to 169.254.2.2:3260
  findmnt /var/lib/spinifex          # mounted by UUID, with _netdev
  ip -br addr show br-wan            # UP, holding the second VNIC address as /32
  ip rule show | grep 200            # source-routing rule for that address
  ip route show table 200            # default via the VCN router, on br-wan
  sudo iptables -S INPUT | head      # 3000 / 8443 / 9999 accepted at the head
'
```

**`_netdev` is not optional and this is where to notice it missing.** An OCI block volume is iSCSI. Without `_netdev` the mount is attempted before `iscsid` has a session, and the boot either hangs or silently lands the whole stack on the small boot disk — which looks identical to a working install until the disk fills. Verify with `findmnt`, never `ls`.

**The default route stays on the *primary* VNIC.** `br-wan` carries its address as a `/32` and its default route in table 200, reached by a source-routing rule. That is not a quirk of the bridge — it is how both VNICs share one subnet without the second one stealing the first one's traffic. `ip route | grep default` on a healthy node names the primary interface, not `br-wan`.

### Step 4. Install Spinifex

Per node, then form the cluster. This is the standard install path — see [Multi-Node Installation](/docs/install-multi-node) for the full walkthrough of formation, which is unchanged on OCI except for the flags called out here.

```bash
curl -sfL https://install.mulgadc.com | bash
sudo /usr/local/share/spinifex/setup-ovn.sh --management --nat-uplink
```

**`--management` is load-bearing.** Omitting it takes the compute-node branch, which stops *and disables* `ovn-central` — silently, leaving a node with no record of what it was meant to be.

**`--nat-uplink`, not `--wan-bridge=br-wan`.** The two are mutually exclusive, and only `--nat-uplink` creates the `spx-nat` transit veth that routed mode runs on; `--wan-bridge` takes the veth-to-`br-ext` branch and deletes `spx-nat` on the way, after which `host.Routed.EnsureUplinkPort` refuses with *"not on OVS — run setup-ovn.sh --nat-uplink"*.

**That does not make `br-wan` redundant.** It is still doing two jobs, neither of which involves OVS:

1. It **names the VNIC** for the OCI allocator. `oci_vnic_iface = "br-wan"` in the pool config is resolved to a VNIC OCID by MAC, per node, which is what lets one identical config file work on every node.
2. It **holds the addresses.** Every external address Spinifex registers becomes a secondary private IP on that VNIC, appears on `br-wan`, and gets its own source-routing rule into table 200. The guests' own traffic reaches it through `spx-nat` and the host's `spinifex-nat-egress` masquerade.

So a correct OCI node has `br-wan` as a **Linux** bridge with no OVS port at all, and `br-ext` as an OVS bridge whose ports are `spx-nat-ovs` and the patch to `br-int`.

On the first node:

```bash
sudo spx admin init --node node1 --nodes 3 --region ap-southeast-2 \
    --external-mode=nat --ipsec=false
```

- **`--external-mode=nat` is mandatory.** See [the consequence](#the-consequence-routed-mode-is-mandatory). `pool` mode produces a healthy-looking cluster that drops every guest packet.
- **`--ipsec=false`:** `openvswitch-ipsec` is typically masked on an OCI image while init defaults the flag to true.
- `--nodes 1` for a single node; the rest of the formation is identical to bare metal.

### Step 5. Configure the OCI pool and IMDS remap

Both are per node. Continue at [Configure Spinifex for OCI](#step-5-configure-spinifex-for-oci) below — the configuration is the same whichever way the infrastructure was built.

---

## Manual deployment

Use this when you have an OCI instance already, or when you want to see what Terraform is doing. It builds one node by hand. For more than one node, build the infrastructure with [Terraform](#terraform-deployment) and follow [Multi-Node Installation](/docs/install-multi-node) for the formation.

> [!TIP]
> **Prefer Terraform — it does all of this for you, identically on every node, and it is what the reference deployment runs.** Every step below is a hand-written version of something `scripts/terraform/oci-spx` already does: the volume and its fstab entry, the second VNIC and `br-wan`, the firewall. Each is a place to get it subtly wrong on node 2 and not find out for a week. Do this to learn it, or when the instance is not yours to rebuild.

Start from an instance with **two VNICs in the same subnet**, both with a public IP, and a block volume attached. The `oci` CLI equivalents, if you are building it rather than adopting it:

```bash
oci compute volume-attachment attach --type iscsi --instance-id <instance-ocid> \
    --volume-id <volume-ocid> --device /dev/oracleoci/oraclevdb
oci compute instance attach-vnic --instance-id <instance-ocid> --subnet-id <subnet-ocid> \
    --assign-public-ip true
```

### Step 1. Attach and mount the data volume over iSCSI

The boot volume is too small for guest images and block storage, and everything Spinifex stores goes on the data volume: viperblock's EBS extents, predastore's S3 objects and JetStream's state. 256 GiB is the minimum; 1 TB is a reasonable start.

**An OCI block volume is iSCSI, not a disk.** Attaching it in the API creates a target; nothing appears under `/dev` until the host logs in to it. There are two ways to get that login, and only the second survives a reboot on its own:

- **Let the Oracle Cloud Agent do it.** Enable the **Block Volume Management** plugin on the instance and attach with `is_agent_auto_iscsi_login_enabled`. This is what Terraform does, and it is the one to choose.
- **Log in by hand.** The Console and `oci compute volume-attachment get` print the exact `iscsiadm` commands for the attachment, in the form:

  ```bash
  sudo iscsiadm -m node -o new -T <iqn> -p 169.254.2.2:3260
  sudo iscsiadm -m node -o update -T <iqn> -n node.startup -v automatic
  sudo iscsiadm -m node -T <iqn> -p 169.254.2.2:3260 -l
  ```

  **`node.startup automatic` is the line that matters** — without it the session does not come back after a reboot, and the mount below silently does not happen.

Confirm the session and the device before touching a filesystem. The agent logs in asynchronously, so on a fresh boot the device can be up to a few minutes behind the instance:

```bash
sudo iscsiadm -m session                       # a session to 169.254.2.2:3260
lsblk -o NAME,SIZE,TYPE,MOUNTPOINT             # the new device, unmounted
ls -l /dev/oracleoci/                          # stable names, if attached with --device
```

Then format and mount. `/dev/oracleoci/oraclevdb` is the stable name for a volume attached with an explicit device path; without one, use the `/dev/sdX` that `lsblk` shows and accept that it can move between boots:

```bash
DEV=/dev/oracleoci/oraclevdb

blkid "$DEV"                                   # STOP if this prints a TYPE — it is not blank
sudo mkfs.ext4 -L spinifex-data "$DEV"

UUID=$(sudo blkid -s UUID -o value "$DEV")
echo "UUID=$UUID /var/lib/spinifex ext4 defaults,_netdev,nofail 0 2" | sudo tee -a /etc/fstab

sudo mkdir -p /var/lib/spinifex
sudo systemctl daemon-reload
sudo mount -o defaults,_netdev,nofail "$DEV" /var/lib/spinifex
findmnt /var/lib/spinifex                      # verify with findmnt, never ls
```

**Mount `/var/lib/spinifex` itself, by UUID, with `_netdev`.** Three separate decisions, all load bearing:

- **The data directory itself**, rather than `/mnt/something` plus a symlink — one mount instead of a mount and a redirect, and no Spinifex configuration points anywhere unusual.
- **By UUID**, because an iSCSI device name is not stable across attach order or reboots.
- **`_netdev`**, because it is what orders the mount after the network and therefore after `iscsid` has a session. Without it the boot either hangs or **silently lands the whole stack on the small boot disk**, which looks identical to a working install until the disk fills. `nofail` keeps a volume that never arrives from wedging the boot instead.

**Reboot once, now, and check `findmnt` again.** This is the only cheap moment to find out that the fstab entry or the iSCSI login does not survive a restart.

### Step 2. Wire the two VNICs

Two VNICs is the tested shape, and the two planes are deliberately separate:

- **Primary VNIC** carries the host plane — the node's advertised address, the AWS gateway and the OVN encapsulation endpoint. It keeps the default route. **Leave it alone.**
- **Second VNIC** carries the external datapath. Put it in a **Linux bridge** named `br-wan`, owned by netplan, with the bridge MAC cloned to the VNIC's own MAC.

Read the second VNIC's MAC, address, subnet and virtual router out of the instance metadata rather than guessing them — it is the VNIC without the default route:

```bash
curl -sH 'Authorization: Bearer Oracle' http://169.254.169.254/opc/v2/vnics/ | jq .
```

Then write a netplan file **of your own**, not `50-cloud-init.yaml`, which cloud-init rewrites:

```yaml
# /etc/netplan/60-spinifex-wan.yaml   (chmod 0600)
network:
  version: 2
  ethernets:
    enp1s0:                                 # the interface holding the second VNIC's MAC
      dhcp4: false
      dhcp6: false
      mtu: 9000
  bridges:
    br-wan:
      interfaces: [enp1s0]
      macaddress: "02:00:17:01:7e:71"       # the VNIC's own MAC — not optional
      mtu: 9000
      dhcp4: false
      dhcp6: false
      # /32, not /23. Both VNICs are in one subnet, so the on-link route the
      # kernel would derive from a prefixed address lands in the main table at
      # metric 0 and takes the whole subnet off the primary VNIC.
      addresses: [10.200.0.29/32]
      routes:
        - to: 10.200.0.0/23
          scope: link
          table: 200
        - to: default
          via: 10.200.0.1
          table: 200
      routing-policy:
        - from: 10.200.0.29/32
          table: 200
          priority: 1000
```

**The `/32` and table 200 are the whole trick.** OCI enforces the source address per VNIC, so a reply that leaves through the primary VNIC carrying a secondary VNIC's address is dropped by the hypervisor. Source routing sends anything sourced from `br-wan`'s address out `br-wan`, and leaves the main routing table — and therefore the default route and your SSH session — untouched. Apply and check:

```bash
sudo netplan apply
ip -br addr show br-wan          # UP, holding the address as /32
ip rule show                     # from <addr> lookup 200
ip route show table 200          # link route for the subnet, default via the VCN router
ip route show default            # still on the PRIMARY interface
```

> [!IMPORTANT]
> **`br-wan` is not given to `setup-ovn.sh` on OCI, and is not bridged into OVS.** In routed mode (Step 4) nothing carries a WAN NIC into `br-ext` at all. `br-wan` earns its place twice over regardless: it is the interface name the OCI allocator is pointed at (`oci_vnic_iface = "br-wan"`, resolved to the VNIC by MAC), and it is where every registered secondary private IP lands and where the host's masquerade sends the guests' traffic. A node in this shape has `br-wan` as a **Linux** bridge with no OVS port, and `br-ext` as an OVS bridge whose only port is the `spx-nat` transit veth. That is correct.

> If you suspect an OVS problem on this host, read `ovs-vsctl --columns=name,error list Interface` and the vswitchd log. **Never trust `ovs-vsctl`'s exit status** — it exits 0 even when the datapath rejected the device.

### Step 3. Open the host firewall

**Ubuntu on OCI defaults to iptables, not nftables**, and its INPUT policy blocks the ports Spinifex serves. Terraform's cloud-init does this on every node; by hand, open them explicitly and persist:

```bash
for p in 3000 8443 9999; do
    sudo iptables -I INPUT 1 -p tcp --dport "$p" -j ACCEPT
done
sudo netfilter-persistent save        # writes /etc/iptables/rules.v4
```

**Do not rely on appending.** Most distro rulesets end in a catch-all REJECT, so an appended ACCEPT lands behind it and does nothing. Insert at the head.

Spinifex installs and manages its own rules; you do not create these by hand. For reference, a working node carries:

| Comment | Table/chain | Purpose |
| --- | --- | --- |
| `spinifex-imds` | `filter` INPUT | Accepts guest IMDS traffic arriving on any `ime-` endpoint |
| `spinifex-imds-remap` | `nat` PREROUTING | DNATs the guest-facing link-local pair to the host-side pair |
| `spinifex-eip-ingress` | `filter` FORWARD | Per-address forward accept, both directions |
| `spinifex-nat-egress` | `nat` POSTROUTING, `filter` FORWARD | Masquerade and conntrack accept for the transit subnet |

`uninstall-spx.sh` removes all four by comment marker. See [Host Firewall](/docs/host-firewall) for the policy Spinifex ships.

### Step 4. Install and form

Identical to [Step 4 of the Terraform path](#step-4-install-spinifex), with `--nodes 1`. Read the notes there on `--management`, `--nat-uplink` and `--external-mode=nat` before running this — all three are decisions you cannot change afterwards without re-forming:

```bash
curl -sfL https://install.mulgadc.com | bash
sudo /usr/local/share/spinifex/setup-ovn.sh --management --nat-uplink
sudo spx admin init --node node1 --nodes 1 --region ap-southeast-2 \
    --external-mode=nat --ipsec=false
```

---

## Step 5. Configure Spinifex for OCI

Both paths meet here. Everything below is per node.

### Discover the OCIDs

```bash
# Instance, compartment and both VNICs, straight from the instance metadata.
curl -sH 'Authorization: Bearer Oracle' http://169.254.169.254/opc/v2/instance/ \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print("compartment:", d["compartmentId"]); print("instance:", d["id"])'

curl -sH 'Authorization: Bearer Oracle' http://169.254.169.254/opc/v2/vnics/ \
  | python3 -c 'import json,sys
for v in json.load(sys.stdin):
    print(v["macAddr"], v["vnicId"], v["subnetCidrBlock"], v.get("privateIp"))'
```

**Pick the VNIC by MAC**, matching the MAC on the interface in your `br-wan` bridge. Oracle's VNIC ordering is not a documented contract, so "the second one" is not a selector. The subnet OCID comes from the VNIC:

```bash
oci network vnic get --vnic-id <vnic-ocid> --query 'data."subnet-id"' --raw-output
```

### Install the credentials on the node

**The daemon cannot read `~/.oci/`.** Its systemd unit sets `ProtectHome=yes`, so a config under any home directory is invisible to it however the permissions read. Credentials go under `/etc/spinifex/`:

```bash
sudo install -d -o root -g spinifex -m 0750 /etc/spinifex/oci
sudo install -o root -g spinifex -m 0640 ~/.oci/oci_api_key.pem /etc/spinifex/oci/oci_api_key.pem
sudo tee /etc/spinifex/oci/config >/dev/null <<'EOF'
[spinifex]
user=ocid1.user.oc1..aaaa...
fingerprint=aa:bb:cc:...
key_file=/etc/spinifex/oci/oci_api_key.pem
tenancy=ocid1.tenancy.oc1..aaaa...
region=ap-sydney-1
EOF
sudo chown root:spinifex /etc/spinifex/oci/config
sudo chmod 0640 /etc/spinifex/oci/config

oci --config-file ~/.oci/config iam region list --query 'data[0].name' --raw-output
```

Keep a copy at `~/.oci/config` too — that is what the `oci` CLI reads for the commands above, and it is a different consumer from the daemon.

### Configure the OCI pool

Add to `/etc/spinifex/spinifex.toml`:

```toml
[[network.external_pools]]
name               = "oci-public"
source             = "oci"
oci_compartment_id = "ocid1.compartment.oc1..aaaa..."
oci_vnic_iface     = "br-wan"
oci_subnet_id      = "ocid1.subnet.oc1.ap-sydney-1.aaaa..."
oci_config_file    = "/etc/spinifex/oci/config"
oci_config_profile = "spinifex"
dns_servers        = ["169.254.169.253"]
```

Notes on the keys:

- **Exactly one of `oci_vnic_id` and `oci_vnic_iface`**, never both — the config is rejected if you set both or neither. Two that disagreed would send allocations to a VNIC the datapath is not on, and OCI would drop the traffic without a word.
- **`oci_vnic_iface` is the right choice on a cluster**, because it resolves through instance metadata **by MAC** on the node it runs on, so the same line is correct on every node. Naming either `br-wan` or the physical interface resolves to the same VNIC. Use `oci_vnic_id` only when you want to pin a specific VNIC on a single node.
- `oci_compartment_id` is the only other required key. `oci_subnet_id` is optional and defaults to the VNIC's own subnet; set it only when you want private IPs from a different subnet. `oci_config_file` defaults to `~/.oci/config` and `oci_config_profile` to `DEFAULT` — **on a node both need setting**, because the daemon cannot read a home directory.
- `oci_public_ip_pool` takes a BYOIP pool OCID. Accepted today so BYOIP is a config change later rather than a code change; see [Limits](#limits-in-this-version).
- `range_start`, `range_end`, `gw_lrp_range_*`, `bind_bridge` and `dhcp_mac` are **rejected** on an OCI pool. OCI owns the addresses; a range you wrote would be fiction.
- Leave the `nat-transit` pool alone. In routed mode the per-VPC gateway router addresses come from the RFC 6598 transit range, not from OCI — **only public addresses handed to guests consume an OCI address.** Fifty VPCs and three Elastic IPs cost three reserved public IPs, not fifty-three.

### Configure the IMDS host addresses

Required on OCI. See [the next section](#the-169254169254-collision) for why.

```toml
[network]
imds_host_meta_ip = "169.254.42.254"
imds_host_dns_ip  = "169.254.42.253"
```

Then start:

```bash
sudo systemctl start spinifex.target
```

---

## The `169.254.169.254` collision

Spinifex serves guest instance metadata on `169.254.169.254` and VPC DNS on `169.254.169.253`, because that is what an AWS-compatible guest expects. **OCI uses `169.254.169.254` for its own instance metadata**, and the host needs it — the identity certificate and the iSCSI boot path both depend on it.

Without the remap, the first guest launch puts `169.254.169.254/32` on a per-tap endpoint, `ip route get 169.254.169.254` starts returning `local … dev lo`, and the host's own metadata service stops answering.

**The guest keeps addressing `169.254.169.254`.** That is not negotiable — it is what AWS compatibility means. The remap is entirely host-side, and it is safe because the IMDS datapath is OpenFlow-steered rather than routed: the ARP responder, the ingress demux and the egress flow all match the guest-facing addresses and are untouched. Only two things change:

1. The per-tap `ime-` endpoint takes the host-side pair from config instead of the guest-facing pair.
2. A PREROUTING DNAT on `-i ime-+` translates the guest-facing pair to the host-side pair. Conntrack un-NATs the reply, so nothing on the return path needs to know.

Pick any unused link-local pair. `169.254.42.254` / `169.254.42.253` is the tested choice. **Set both keys or neither** — a half-configured pair is ignored, and the node behaves as if the remap were off.

Verify after the first guest launches:

```bash
ip route get 169.254.169.254                 # must be your real NIC, not lo
curl -sH 'Authorization: Bearer Oracle' -o /dev/null -w '%{http_code}\n' \
     http://169.254.169.254/opc/v2/instance/ # must be 200
ip -br addr show | grep ime-                 # endpoint holds 169.254.42.x, not .169.x
sudo iptables -t nat -S | grep spinifex-imds-remap
```

---

## Verify end to end

```bash
export AWS_PROFILE=<your-profile>

aws ec2 allocate-address
aws ec2 run-instances --image-id <ami> --instance-type t3.micro --key-name <key> \
    --subnet-id <subnet> --associate-public-ip-address
aws ec2 describe-instances --query \
    'Reservations[].Instances[].[InstanceId,PrivateIpAddress,PublicIpAddress,State.Name]' \
    --output text
```

Then, from the host:

```bash
ping -c 3 <public-ip>
ssh -i <key> ubuntu@<public-ip> hostname
```

And from inside the guest — note that IMDS is **IMDSv2, so it needs a token**; an untokened request correctly returns 401:

```bash
TOK=$(curl -s -X PUT http://169.254.169.254/latest/api/token \
      -H 'X-aws-ec2-metadata-token-ttl-seconds: 120')
curl -s -H "X-aws-ec2-metadata-token: $TOK" \
     http://169.254.169.254/latest/meta-data/instance-id
traceroute -n 8.8.8.8
```

A healthy topology is three hops before OCI's network: the OVN router port, the transit gateway, then upstream.

On a cluster, prove an address survives a move. `scripts/terraform/oci-spx/eip-move-test.sh` does the whole sequence — it asserts that an auto-assigned address is returned to OCI on stop and a different one comes back, then associates an Elastic IP, forces the guest to start on another node, and checks OCI's own record of which VNIC carries the private half:

```bash
INSTANCE=i-... ./scripts/terraform/oci-spx/eip-move-test.sh
```

### What a correct node looks like

Worth knowing so the asymmetry does not read as a fault:

```bash
sudo ovn-nbctl lr-nat-list <logical-router>    # dnat_and_snat holds the PRIVATE address
aws ec2 describe-instances ...                 # reports the PUBLIC address
ip route show | grep spx-nat-host              # route to the PRIVATE address
```

**These disagreeing is the design.** The public address is 1:1-NATed by OCI upstream and never touches the wire, so every datapath object holds the private half while every AWS-facing record holds the public half. If OVN showed the public address, the rule would be matching something that never arrives.

---

## Oracle Linux as a guest image

Spinifex ships four Oracle Linux entries in its image catalog, so a customer on OCI can run the same distro their Oracle support contract covers. They import exactly like any other AMI — nothing about them is OCI-specific, and they run equally well on bare metal:

| Catalog name | Release | Arch | Kernel |
| --- | --- | --- | --- |
| `oracle-10.1-x86_64` | Oracle Linux 10.1 | x86_64 | UEK 8 |
| `oracle-10.1-arm64` | Oracle Linux 10.1 | arm64 | UEK 8 |
| `oracle-9.8-x86_64` | Oracle Linux 9.8 | x86_64 | UEK 7 |
| `oracle-9.8-arm64` | Oracle Linux 9.8 | arm64 | UEK 7 |

```bash
sudo spx admin images import --name oracle-10.1-x86_64 --config /etc/spinifex/spinifex.toml
aws ec2 describe-images --query 'Images[].[Name,State,BootMode]' --output text
```

All four boot **UEFI**, so launch them into a shape that boots UEFI — a BIOS-only instance type will not come up, and the failure looks like a hung boot rather than a rejected image.

### The login user is `cloud-user`, not `opc`

```bash
ssh -i path/to/key.pem cloud-user@<public-ip>
```

**This is the one that catches people.** `opc` is the default user on the images Oracle publishes *into OCI itself*; these are the generic **KVM cloud images** from `yum.oracle.com`, whose cloud-init `default_user` is `cloud-user`. Verified on both releases: `opc`, `oracle` and `ec2-user` are all refused. The Spinifex UI's instance detail page shows the right user per AMI, so read it there rather than guessing from the distro.

### Why these four pin a checksum digest inline

Every other catalog entry verifies against the publisher's sums file. Oracle ships none — the digests are published as HTML on `yum.oracle.com/oracle-linux-templates.html` — so these four carry `ChecksumDigest` in `spinifex/utils/images.go` instead. That is safe **only** because each URL names an immutable build (`b291`, `b293`, `b178`, `b182`) and Oracle never moves one. A new point release is a new URL and a new digest, never an edit to an existing entry.

On arm64 Oracle publishes a `-kvm-cloud-` build alongside a plain `-kvm-` one. The catalog takes `-kvm-cloud-`: it is the one with cloud-init, and so the one that matches x86_64's `-kvm-`. The plain arm64 build imports fine and then has no datasource, which surfaces as an instance with no key and no metadata rather than as an import error.

### SELinux is enforcing

Both releases ship `SELinux: enforcing`, unlike the Debian and Ubuntu images. That is the upstream default and Spinifex does not change it. It does not affect networking, IMDS or cloud-init — all verified working — but a workload that has only ever run on Debian may meet it for the first time here.

---

## Troubleshooting

### Addresses and quotas

**Spinifex enforces no limit of its own.** It attempts the allocation and reports what OCI says, so the ceiling you hit is always your tenancy's, never a number baked into the software. A quota is raised with Oracle, not reconfigured here, and there is nothing to restart afterwards.

**`InsufficientAddressCapacity` on `allocate-address` is what exhaustion looks like.** It is the AWS error code for "out of addresses" and Spinifex returns it for either quota, mapped from OCI's own refusal. On a cluster the regional one is the likelier of the two.

**A detached reserved public IP still bills.** `disassociate-address` returns the address to `AVAILABLE` without deleting it, exactly as an unassociated AWS Elastic IP behaves, and it keeps consuming your regional quota as well as your bill. `release-address` is what frees both.

**A stopped instance costs nothing for its auto-assigned address.** Stopping an instance returns its auto-assigned address to the pool and deletes the reserved public IP object behind it, as AWS does; the start that follows creates a new one. An **Elastic IP** is yours and survives the stop untouched — that is the whole distinction between the two, and on OCI it is also the difference between a stopped fleet that bills for addresses and one that does not.

### Datapath

**Instance launches but the public address is unreachable.** In order:

1. `sudo ovn-nbctl lr-nat-list <router>` — is there a `dnat_and_snat` row, and does it hold the *private* address?
2. `ip route get <private-addr>` — does it route via the transit veth?
3. `sudo iptables -S FORWARD | grep spinifex-eip-ingress` — both directions present?
4. `oci network private-ip list --vnic-id <vnic>` — is the private address actually registered with OCI, **and on this node's VNIC**? If not, nothing else matters.

**Egress works from one address and not another on the same NIC.** That is [source-IP enforcement](#what-oci-changes), and it means the address is not a registered private-IP object on the VNIC. It is never a routing problem on the host.

**An address went dark after a guest moved node.** The affinity pass should have claimed it within 15 seconds. Check the new node's daemon journal for `ocinet affinity`, then confirm with step 4 above which VNIC OCI thinks holds the private half.

### Host

**`RunInstances` returns `ServerInternal`.** Check the daemon journal for the real error — `journalctl -u spinifex-daemon --since -10m`. An OCI allocation failure surfaces this way.

**Guests boot but cloud-init hangs on metadata.** That is [the `169.254.169.254` collision](#the-169254169254-collision). `ip route get 169.254.169.254` returning `dev lo` is the signature.

**The host loses its own metadata service after a guest terminates.** A known teardown leak: an `ime-` endpoint can survive guest termination still holding its address. With the remap configured this is harmless; without it, the host's metadata route is captured. Remove the stale OVS port by hand and configure the remap.

**The stack ends up on the boot disk and the node fills.** The data volume was mounted without `_netdev` and lost the race with `iscsid`. `findmnt /var/lib/spinifex` is the check; `ls` cannot tell the difference.

---

## Limits in this version

- **Operator-provisioned API key**, not instance principal. The key is on each node's disk and its rotation is your responsibility.
- **One VNIC per pool.** A pool cannot grow past the per-VNIC secondary private IP limit of 64. Three nodes give you three pools' worth; a single node is capped at 64 addresses.
- **IPv4 only.** IPv6 on OCI is materially simpler — no pools, prefixes assign directly to VCNs — but is not implemented.
- **No BYOIP.** The integration accepts a public IP pool OCID in config so BYOIP becomes a config change rather than a code change, but the RIR validation and Oracle's own validation window both precede any use of it.
- **OCI Block Volumes are not an EBS provider.** They back `/var/lib/spinifex` and therefore viperblock, which captures most of the performance benefit, but there is no native OCI block provider.
- **Terraform builds infrastructure, not Spinifex.** Formation is still the standard install path, by design: it is the same path prod and bare metal take, and diverging it for one cloud would mean two formation paths to keep correct.

### What is no longer a limit

Recorded because earlier versions of this guide said otherwise:

- **Multi-node works.** The `--nodes=1` cap on routed mode is gone, three-node formation and guest-to-guest traffic are proven on OCI, and the routed-NAT datapath is guarded nightly on three nodes.
- **Public addresses are not pinned to one node.** Each node allocates on its own VNIC, and an affinity pass reassigns an address's private half to whichever node the guest is on — one OCI call, keeping the OCID, so the customer's public address never changes. No node needs another node's VNIC OCID, and `oci_vnic_iface` is correct on every node.
- **Addresses are released when an instance stops.** An auto-assigned address goes back to the pool on stop and the reserved public IP object is deleted, so a stopped fleet stops costing you addresses.
