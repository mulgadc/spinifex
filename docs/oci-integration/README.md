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
- [How it fits together](#how-it-fits-together)
- [Prerequisites](#prerequisites)
- [Option A — Deploy with Terraform](#option-a--deploy-with-terraform)
- [Option B — Manual single-node install](#option-b--manual-single-node-install)
- [The `169.254.169.254` collision](#the-169254169254-collision)
- [Verify end to end](#verify-end-to-end)
- [Oracle Linux as a guest image](#oracle-linux-as-a-guest-image)
- [Troubleshooting](#troubleshooting)
- [Limits in this version](#limits-in-this-version)

---

## Overview

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

```text
                                  Internet
                                      │
                    ┌─────────────────┴──────────────────┐
                    │  OCI VCN edge — Internet Gateway   │
                    │  RESERVED public IP  ⇄  1:1 NAT    │
                    │  ⇅ secondary private IP on a VNIC  │
                    └─────────────────┬──────────────────┘
                                      │  subnet 10.200.0.0/23
 ┌────────────────────────────────────┴───────────────────────────────────┐
 │  BM.Standard.E5.192 — one OCI instance                                 │
 │                                                                        │
 │   VNIC 0 · 10.200.0.10            VNIC 1 · 10.200.0.11                 │
 │   host plane: SSH, advertise,     external datapath                    │
 │   Geneve encap endpoint           secondaries: .40 .41 .42  ← the EIPs │
 │          │                                  │                          │
 │          │                            br-wan (Linux bridge, netplan)   │
 │          │                                  │                          │
 │          │                            spx-nat  (RFC 6598 transit veth) │
 │          │                                  │                          │
 │   ┌──────┴──────────────────────────────────┴───────────────────────┐  │
 │   │ Spinifex control plane                                          │  │
 │   │  awsgw :9999   ui :3000   predastore :8443                      │  │
 │   │  nats · daemon · vpcd · viperblock · northstar                  │  │
 │   └──────────────────────────┬──────────────────────────────────────┘  │
 │                              │  OVN br-int                             │
 │   ┌──────────────────────────┴──────────────────────────────────────┐  │
 │   │  Tenant VPCs — the AWS surface, entirely inside this host        │  │
 │   │                                                                  │  │
 │   │   vpc-a · 10.0.0.0/16              vpc-b · 10.1.0.0/16           │  │
 │   │    ├─ i-aaa  10.0.1.5  ⇄ EIP on .40                              │  │
 │   │    └─ i-bbb  10.0.1.6   (private, egress via routed NAT)         │  │
 │   │                                     └─ i-ccc 10.1.1.5 ⇄ EIP .41  │  │
 │   │                                                                  │  │
 │   │   EBS volumes → viperblock → predastore (S3) on the data volume  │  │
 │   └──────────────────────────────────────────────────────────────────┘ │
 └────────────────────────────────────────────────────────────────────────┘
```

Two things in that picture are the whole integration. The guest's private address (`10.0.1.5`) never leaves the host — OVN and the host route both hold it. The address OCI delivers to (`10.200.0.40`) is a **secondary private IP registered on VNIC 1**, and the customer's public address is a reserved public IP that OCI 1:1-NATs onto it upstream. `describe-instances` reports the public one; every datapath object holds the private one.

### Three nodes — the same surface, distributed

Each node registers the addresses for **its own** guests on **its own** second VNIC, and the allocator runs per node, so nothing has to know another node's OCIDs. When a guest moves — a stop/start that lands elsewhere, or a host failure — the node it lands on claims the address's private half from the old VNIC through a single OCI call. The OCID and the public half are untouched, so the customer's address never changes.

```text
                                   Internet
                                       │
                     ┌─────────────────┴──────────────────┐
                     │ OCI VCN edge · Internet Gateway    │
                     │ reserved public IPs 1:1-NATed to   │
                     │ secondary private IPs, per VNIC    │
                     └───┬───────────────┬────────────┬───┘
       subnet 10.200.0.0/23 ─────────────┴────────────┴────────────────
             │                      │                       │
  ┌──────────┴─────────┐ ┌──────────┴─────────┐ ┌───────────┴────────┐
  │ node1              │ │ node2              │ │ node3              │
  │ VNIC0 .10  VNIC1 .11│ │ VNIC0 .20  VNIC1 .21│ │ VNIC0 .30 VNIC1 .31│
  │   secondaries .40   │ │   secondaries .41   │ │   secondaries .42  │
  │                    │ │                    │ │                    │
  │ awsgw ui predastore│ │ awsgw ui predastore│ │ awsgw ui predastore│
  │ nats · vpcd · daemon│ │ nats · vpcd · daemon│ │ nats · vpcd ·daemon│
  │ ovn-central (raft) │ │ ovn-central (raft) │ │ ovn-central (raft) │
  │                    │ │                    │ │                    │
  │  i-aaa ⇄ EIP .40   │ │  i-bbb ⇄ EIP .41   │ │  i-ccc ⇄ EIP .42   │
  └─────────┬──────────┘ └─────────┬──────────┘ └─────────┬──────────┘
            │                      │                      │
            └──── Geneve overlay (UDP 6081) over VNIC 0 ──┘
            └──── predastore RS(2,1) shards · NATS · OVN raft ────┘

  A guest moving node2 → node3 takes its address with it: node3's next
  affinity pass reassigns the private half from VNIC1 .21 to VNIC1 .31
  in one OCI call. The reserved public IP is never touched.
```

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

## Option A — Deploy with Terraform

**This is the preferred path, for one node or three.** It builds the VCN, gateways, subnets and security lists, then each node with its two VNICs, its data volume and the host prerequisites — the `br-wan` bridge, the iSCSI mount and the open service ports — through cloud-init. What it does not do is install Spinifex; that is Step 4 below, and it is the same install path prod and bare metal use.

The configuration lives in [`scripts/terraform/oci-spx`](../../scripts/terraform/oci-spx/README.md), which has the full variable reference and the resource-by-resource architecture.

### Step 1. Set your inputs

Prerequisites: Terraform, Python 3, OCI credentials in `~/.oci/config`, and an SSH public key. The Python helper builds and uses the configuration's own `.venv`.

```bash
cd scripts/terraform/oci-spx
cat > terraform.auto.tfvars <<'EOF'
compartment_ocid            = "ocid1.compartment.oc1..aaaa..."
node_count                  = 3          # 1 for a single node
compute_shape               = "VM.Standard.E6.Flex"
compute_ocpus               = 8          # OCI counts an OCPU as a full core
compute_memory_in_gbs       = 32
data_volume_size_in_gbs     = 256
node_client_cidr_allow_list = ["203.0.113.10/32"]
EOF
```

> [!WARNING]
> **`node_client_cidr_allow_list` defaults to `0.0.0.0/0`.** Nodes sit in a public subnet with public addresses because they serve the UI, the S3 gate and the AWS gateway directly — there is no bastion. Restrict this before any real use.

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
  findmnt /var/lib/spinifex          # data volume, mounted by UUID with _netdev
  ip -br link show br-wan            # WAN bridge exists and is UP
  ip route | grep default            # default route is on br-wan
  sudo iptables -S INPUT | head      # 3000 / 8443 / 9999 accepted at the head
'
```

**`_netdev` is not optional and this is where to notice it missing.** An OCI block volume is iSCSI. Without `_netdev` the mount is attempted before `iscsid` has a session, and the boot either hangs or silently lands the whole stack on the small boot disk — which looks identical to a working install until the disk fills. Verify with `findmnt`, never `ls`.

### Step 4. Install Spinifex

Per node, then form the cluster. This is the standard install path — see [Multi-Node Installation](/docs/install-multi-node) for the full walkthrough of formation, which is unchanged on OCI except for the flags called out here.

```bash
curl -sfL https://install.mulgadc.com | bash
sudo /usr/local/share/spinifex/setup-ovn.sh --management --nat-uplink
```

**`--management` is load-bearing.** Omitting it takes the compute-node branch, which stops *and disables* `ovn-central` — silently, leaving a node with no record of what it was meant to be.

**`--nat-uplink`, not `--wan-bridge=br-wan`.** The two are mutually exclusive, and only `--nat-uplink` creates the `spx-nat` transit veth that routed mode runs on; `--wan-bridge` takes the veth-to-`br-ext` branch and deletes `spx-nat` on the way, after which `host.Routed.EnsureUplinkPort` refuses with *"not on OVS — run setup-ovn.sh --nat-uplink"*. `br-wan` still exists and still holds the addresses — in routed mode nothing is bridged into `br-ext`, which is the whole point of it.

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

## Option B — Manual single-node install

Use this when you have an OCI instance already, or when you want to understand what Terraform is doing. It builds one node by hand. For more than one node, build the infrastructure with [Option A](#option-a--deploy-with-terraform) and follow [Multi-Node Installation](/docs/install-multi-node) for the formation.

### Step 1. Attach and mount the data volume

The boot volume is too small for guest images and block storage. Attach a block volume (256 GiB minimum, 1 TB a reasonable start) and mount it at `/var/lib/spinifex`, which puts viperblock, predastore and JetStream on it with no config pointing anywhere unusual.

```bash
sudo mkfs.ext4 /dev/sdb1
UUID=$(sudo blkid -s UUID -o value /dev/sdb1)
echo "UUID=$UUID /var/lib/spinifex ext4 defaults,_netdev,nofail 0 2" | sudo tee -a /etc/fstab
sudo mkdir -p /var/lib/spinifex && sudo mount -a
findmnt /var/lib/spinifex          # verify with findmnt, never ls
```

**`_netdev` is not optional** — see Step 3 of Option A for why.

### Step 2. Wire the two VNICs

Two VNICs is the tested shape:

- **Primary VNIC** carries the management plane — the node's advertised address and the OVN encapsulation endpoint.
- **Second VNIC** carries the external datapath. Put it in a **Linux bridge** (`br-wan`) owned by netplan, with the bridge MAC cloned to the VNIC's own MAC.

```yaml
# /etc/netplan/50-cloud-init.yaml (fragment)
bridges:
  br-wan:
    interfaces: [enp1s0]
    macaddress: "02:00:17:01:7e:71"     # the VNIC's own MAC — not optional
    addresses: [10.200.0.29/24]
```

`setup-ovn.sh` detects that `br-wan` is a *Linux* bridge and links it to OVS with a veth pair rather than enslaving the NIC. **That is the correct shape on OCI, not a workaround** — the physical interface never becomes an OVS port, so the VNIC keeps its MAC identity and netplan keeps owning the addresses.

> If you suspect an OVS problem on this host, read `ovs-vsctl --columns=name,error list Interface` and the vswitchd log. **Never trust `ovs-vsctl`'s exit status** — it exits 0 even when the datapath rejected the device.

### Step 3. Open the host firewall

**Ubuntu on OCI defaults to iptables, not nftables**, and its INPUT policy blocks the ports Spinifex serves. Open them explicitly and persist:

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

Identical to Step 4 of Option A, with `--nodes 1`:

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

**Egress works from one address and not another on the same NIC.** That is [source-IP enforcement](#overview), and it means the address is not a registered private-IP object on the VNIC. It is never a routing problem on the host.

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
