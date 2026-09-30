---
title: "Spinifex on Exoscale"
seoTitle: "Run Spinifex on Exoscale with Elastic IPs — Spinifex Docs"
description: "Install Spinifex on one Exoscale instance and run EC2, load balancers and EKS on real Exoscale Elastic IPs."
category: "Install"
tags:
  - install
  - exoscale
  - cloud
  - elastic ip
resources:
  - title: "Single-Node Install"
    url: "/docs/install"
  - title: "Spinifex on Oracle Cloud"
    url: "/docs/oci-integration"
  - title: "VPC Networking"
    url: "/docs/vpc-networking"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
---

# Running Spinifex on Exoscale

> [!WARNING]
> **Work in progress.** This page is not yet published on docs.mulgadc.com. Do not add it to `scripts/docs-config.json` in `mulga-docs` until the [Limits](#limits) section is settled.

> One Exoscale instance, the AWS APIs on its address, and guests reachable on real Exoscale Elastic IPs that Spinifex creates and deletes for you.

## Table of Contents

- [What you get](#what-you-get)
- [Before you start](#before-you-start)
- [Install](#install)
- [Use it from your laptop](#use-it-from-your-laptop)
- [Troubleshooting](#troubleshooting)
- [Limits](#limits)

---

## What you get

A single Exoscale instance runs the whole Spinifex stack: EC2, EBS, S3, VPC, load balancers and EKS, behind an endpoint the ordinary AWS CLI, SDKs and Terraform provider talk to unmodified.

Whenever something needs a public address, such as an instance in a public subnet, an Elastic IP, an internet-facing load balancer or a NAT gateway, Spinifex creates an Exoscale Elastic IP, attaches it to your instance, and deletes it again when the resource goes away. You never manage the addresses by hand.

<p align="center">
  <img src="../../.github/assets/diagrams/exoscale-single-node.svg" alt="Single node on Exoscale — the instance on the zone's public network with one interface, Elastic IPs routed into OVN through the routed-NAT transit veth, the control plane, tenant VPCs and the block storage volume" width="900">
</p>

---

## Before you start

You need:

- An Exoscale organisation and an **API key** that can create, attach, detach and delete Elastic IPs.
- An SSH key pair.
- The AWS CLI on your laptop.

Then create these in one zone, from the Exoscale console or the `exo` CLI. This guide uses `de-fra-1`.

| Resource | Setting |
| --- | --- |
| Compute instance | `standard.huge` (8 vCPU / 32 GiB) or larger, template `Linux Ubuntu 26.04 LTS 64-bit`, 100 GiB disk, your SSH key |
| Block storage volume | 256 GiB or more, attached to the instance |
| Security group | Allow all inbound TCP, UDP and ICMP from `0.0.0.0/0`, attached to the instance |

Note the instance's **ID** (a UUID, shown on its page in the console) and the volume's **ID**. You do not need to create any Elastic IPs.

> [!NOTE]
> The security group is open on purpose. Guests are filtered by their own Spinifex security groups, and the node by the Spinifex host firewall you turn on in step 5.

---

## Install

Everything below runs on the instance, over SSH as `ubuntu`.

### 1. Check that the instance can run VMs

```bash
ls -l /dev/kvm && cat /sys/module/kvm_intel/parameters/nested
```

`/dev/kvm` must exist and the second line must print `Y`. If either fails, pick a different instance type before going further.

### 2. Put the data volume at `/var/lib/spinifex`

Every guest disk and S3 object lives here, so it belongs on the volume and not on the 100 GiB root disk.

```bash
VOL_ID=<volume ID>
DEV=/dev/disk/by-id/virtio-${VOL_ID:0:20}

sudo mkfs.ext4 -L spinifex-data "$DEV"          # only on a new, empty volume
sudo mkdir -p /var/lib/spinifex
echo "UUID=$(sudo blkid -o value -s UUID "$DEV") /var/lib/spinifex ext4 defaults,nofail 0 2" | sudo tee -a /etc/fstab
sudo mount /var/lib/spinifex
findmnt /var/lib/spinifex                       # must print a line
```

### 3. Install the `exo` CLI and give it your key

Spinifex manages Elastic IPs by running `exo`, so it needs the CLI and a key file.

```bash
curl -fsSLO https://github.com/exoscale/cli/releases/download/v1.101.0/exoscale-cli_1.101.0_linux_amd64.deb
sudo dpkg -i exoscale-cli_1.101.0_linux_amd64.deb

sudo mkdir -p /etc/spinifex/exoscale
sudo tee /etc/spinifex/exoscale/exoscale.toml >/dev/null <<'EOF'
defaultaccount = "spinifex"

[[accounts]]
  name        = "spinifex"
  key         = "EXO..."
  secret      = "..."
  defaultZone = "de-fra-1"
EOF
sudo chmod 700 /etc/spinifex/exoscale && sudo chmod 600 /etc/spinifex/exoscale/exoscale.toml
```

Check the key works: `sudo exo --config /etc/spinifex/exoscale/exoscale.toml compute elastic-ip list -z de-fra-1`.

### 4. Install Spinifex

```bash
curl -sfL https://install.mulgadc.com | sudo bash
sudo /usr/local/share/spinifex/setup-ovn.sh --management --nat-uplink

sudo spx admin init --node node1 --nodes 1 --region eu-central-1 --az eu-central-1a \
  --ipsec=false --external-mode=nat
sudo chown -R spinifex-daemon: /etc/spinifex/exoscale
```

Now tell Spinifex where to get public addresses. Add this to the end of `/etc/spinifex/spinifex.toml`, just above the `[bootstrap]` section, using your instance ID:

```toml
[[network.external_pools]]
name                 = "exo"
source               = "exoscale"
exoscale_zone        = "de-fra-1"
exoscale_instance_id = "<instance ID>"
```

Leave the `nat-transit` pool that `init` wrote where it is.

> [!NOTE]
> `eu-central-1` is the region name your AWS tools will see. It is your choice and does not have to match the Exoscale zone.

### 5. Turn on the firewall, then start

```bash
sudo env SETUP_STAGES=firewall /usr/local/share/spinifex/setup.sh --firewall on
sudo systemctl start spinifex.target
systemctl list-units 'spinifex-*' --no-pager
```

Every `spinifex-*` unit should be `active`. The daemon log confirms the Elastic IP integration is working:

```bash
sudo journalctl -u spinifex-daemon | grep -E 'exo CLI|Exoscale allocator ready'
```

### 6. Import an image

```bash
sudo spx admin images import --name ubuntu-26.04-x86_64 --config /etc/spinifex/spinifex.toml
```

For EKS, also import `spinifex-eks-node`. `sudo spx admin images list --config /etc/spinifex/spinifex.toml` shows the full catalogue.

---

## Use it from your laptop

Copy the cluster's CA and the admin credentials from the instance:

```bash
NODE=<instance public IP>
mkdir -p ~/.spinifex
ssh ubuntu@$NODE 'sudo cat /etc/spinifex/ca.pem' > ~/.spinifex/exo-ca.pem
```

Copy the `[spinifex]` section of `~/.aws/credentials` on the instance into your own `~/.aws/credentials` as `[spinifex-exo]`, then:

```bash
aws configure set --profile spinifex-exo region eu-central-1
export AWS_PROFILE=spinifex-exo AWS_CA_BUNDLE=~/.spinifex/exo-ca.pem
export AWS_ENDPOINT_URL=https://$NODE:9999

aws ec2 describe-regions        # proves the connection
```

### Launch an instance

```bash
aws ec2 import-key-pair --key-name demo --public-key-material fileb://~/.ssh/id_ed25519.pub
SG=$(aws ec2 describe-security-groups --filters Name=group-name,Values=default --query 'SecurityGroups[0].GroupId' --output text)
aws ec2 authorize-security-group-ingress --group-id "$SG" --protocol tcp --port 22 --cidr 0.0.0.0/0

AMI=$(aws ec2 describe-images --filters Name=name,Values='*ubuntu-26.04*' --query 'Images[0].ImageId' --output text)
aws ec2 run-instances --image-id "$AMI" --instance-type t3.small --key-name demo \
  --query 'Instances[0].InstanceId' --output text
```

After about a minute, `describe-instances` shows a public IP. That is a new Exoscale Elastic IP, and it appears in the Exoscale console with a description beginning `spinifex:`.

```bash
ssh ubuntu@<public IP> 'curl -s ifconfig.me'    # prints the same address
```

Terminating the instance deletes the Elastic IP at Exoscale.

### Load balancers and EKS

The [Terraform workbooks](https://github.com/mulgadc/spinifex/tree/main/docs/terraform-workbooks) run unchanged. Pass the region and endpoint:

```bash
cd nginx-alb
terraform init
terraform apply -var region=eu-central-1 -var spinifex_endpoint=https://$NODE:9999
```

`nginx-alb` builds two web servers behind an internet-facing load balancer. `eks-quickstart` builds a Kubernetes cluster with one worker. Run `terraform destroy` before starting the next one, because of the quota below.

> [!IMPORTANT]
> **Exoscale allows five Elastic IPs per organisation by default**, and every public address Spinifex hands out uses one. An instance with a public IP uses one, an internet-facing load balancer one, a NAT gateway one, and `eks-quickstart` three. Past the quota, `allocate-address` returns `AddressLimitExceeded` and a launch returns `InsufficientAddressCapacity`. Ask Exoscale support to raise it.
>
> **Exoscale keeps counting a deleted Elastic IP for a while.** After a `terraform destroy`, `exo limits` still shows the old usage even though `exo compute elastic-ip list` no longer shows the addresses, so an `apply` straight afterwards can hit the quota. Wait until `exo limits` agrees with the list before starting the next workbook.

---

## Troubleshooting

**A guest's public IP does not answer from the instance itself.** That is expected: traffic from the host to a guest's Elastic IP is dropped. Test from your laptop.

**SSH or HTTP is refused straight after launch.** Guests take 30 to 60 seconds to boot. Wait and retry.

**An EKS cluster goes to `FAILED` within seconds, or a load balancer never becomes active.** Run `exo limits`. If Elastic IP usage is at the maximum, the cluster could not get its public endpoint; destroy it, wait for usage to drop, and apply again.

**The daemon logs an `exo` error at start.** Run the check at the end of step 3. A wrong key, secret or zone is the usual cause.

**An Elastic IP you created yourself.** Spinifex only ever touches Elastic IPs whose description starts with `spinifex:<instance ID>:`. Anything else in the organisation is left alone, but it still counts against the quota.

---

## Limits

Proven on one `standard.huge` instance in `de-fra-1` on 2026-09-30: EC2 with a public IP, Elastic IP allocate and release, the `nginx-alb` workbook and the `eks-quickstart` workbook with one worker. Each was reached from the internet and left no Elastic IP behind at Exoscale.

Not yet tested:

- **More than one node.** Each node would carry its own guests' Elastic IPs, with cluster traffic over an Exoscale private network.
- **Moving an Elastic IP between nodes** with its guest.
- **A narrowly scoped API key.** The proof used an unrestricted key.
- **Other zones, bare metal and GPU instances.**
- **Guest disk performance** on Exoscale block storage.
