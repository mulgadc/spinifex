---
title: "Spinifex on Vultr"
seoTitle: "Run Spinifex on Vultr with reserved IPs — Spinifex Docs"
description: "Deploy Spinifex on one or three Vultr VMs with a single command, and run EC2, load balancers and EKS on real Vultr reserved IPs."
category: "Cloud Install"
tags:
  - install
  - vultr
  - cloud
  - terraform
  - cluster
resources:
  - title: "Spinifex on Oracle Cloud"
    url: "/docs/oci-integration"
  - title: "Spinifex on Exoscale"
    url: "/docs/exoscale-integration"
  - title: "Spinifex Repository"
    url: "https://github.com/mulgadc/spinifex"
---

# Running Spinifex on Vultr

> [!WARNING]
> **Work in progress.** This page is not yet published on docs.mulgadc.com. Do not add it to `scripts/docs-config.json` in `mulga-docs` until Vultr support is in a published release.

> One command builds Spinifex on Vultr VMs and gives your guests real public addresses. Removing it takes one more command.

## Table of Contents

- [What you get](#what-you-get)
- [Before you start](#before-you-start)
- [Deploy](#deploy)
- [Use it](#use-it)
- [Watch a failover](#watch-a-failover)
- [Tear it down](#tear-it-down)
- [Troubleshooting](#troubleshooting)
- [Limits](#limits)

---

## What you get

`deploy.sh` creates one or three Vultr VMs in a private VPC of their own, installs Spinifex on them and forms the cluster. The AWS APIs (EC2, EBS, S3, VPC, load balancers and EKS) are then served on the first node's address, and the ordinary AWS CLI, SDKs and Terraform provider talk to them unmodified.

Anything that needs a public address gets a Vultr reserved IP, attached to the VM its guest runs on. That covers an instance in a public subnet, an Elastic IP, an internet-facing load balancer and a NAT gateway. Spinifex creates each address and deletes it when the resource goes away.

On three nodes, an instance whose VM fails is restarted on another node, and its address follows it.

---

## Before you start

You need:

- **A Vultr account** with room for the VMs. Each node is a `vx1-g-16c-64g` (16 vCPU, 64 GB), $0.48 an hour, so three nodes cost $1.44 an hour while they run.
- **A Vultr API key.** In the Vultr portal, open **Account → API** and enable it. Under **Access Control**, the key must accept calls from your machine *and* from every node, because the nodes call the API to manage their addresses. The node addresses do not exist until the deploy creates them, so either allow all IPv4 for now or add them when the deploy stops and asks (see [Troubleshooting](#troubleshooting)).
- **On your machine:** [OpenTofu](https://opentofu.org/docs/intro/install/) 1.5 or newer, the AWS CLI v2, `python3`, and an SSH key pair at `~/.ssh/id_ed25519` (or pass `--ssh-key`).
- **The Spinifex repository**, for `deploy.sh`:

```bash
git clone https://github.com/mulgadc/spinifex.git
cd spinifex
git switch dev
cd scripts/terraform/vultr-spx
```

> [!NOTE]
> Vultr support is on the development branch and not yet in a published release. `git switch dev` gives you the deploy driver for it, and `--channel dev` below installs the matching Spinifex build, the pre-release published from every change to `dev`. Once Vultr support is in a release, skip `git switch dev` and pass `--channel latest`.

---

## Deploy

```bash
export VULTR_API_KEY=<your key>

./deploy.sh up --name demo --channel dev              # one node, about 3 minutes
./deploy.sh up --name demo --nodes 3 --channel dev    # three nodes, about 16 minutes
```

`--name` labels everything the deployment creates, so you can run several side by side. The defaults are Vultr region `ord` and Spinifex region `us-east-1`; `./deploy.sh` with no arguments lists the options.

When it finishes, `deploy.sh` prints the API endpoint, `https://<node1 address>:9999`. It has also written an AWS profile called `spinifex-vultr-demo`, with the endpoint and the cluster's CA already set, and imported an Ubuntu 26.04 image. If a step fails, fix the cause and run the same command again: it resumes from the step that failed.

Check it:

```bash
aws --profile spinifex-vultr-demo ec2 describe-images --query 'Images[].Name'
```

To SSH to a node, use the host key the deploy pinned:

```bash
ssh -o UserKnownHostsFile=~/.spinifex/vultr/demo/known_hosts root@<node address>
```

`./deploy.sh status --name demo` lists the completed steps and every VM, volume, VPC and reserved IP the deployment still holds. The node addresses are in the Vultr portal under **Compute**.

---

## Use it

The workbooks in `docs/terraform-workbooks/` are ordinary AWS Terraform. `nginx-alb` builds a VPC, two web servers and an internet-facing load balancer:

```bash
cd ../../../docs/terraform-workbooks/nginx-alb
export AWS_PROFILE=spinifex-vultr-demo AWS_CA_BUNDLE=~/.spinifex/vultr-demo-ca.pem
tofu init
tofu apply -var region=us-east-1 -var spinifex_endpoint=https://<node1 address>:9999

aws elbv2 describe-load-balancers --names nginx-alb \
    --query 'LoadBalancers[0].AvailabilityZones[].LoadBalancerAddresses[].IpAddress' --output text
curl http://<that address>/      # alternates between the two servers
```

The servers take a minute or two after `apply` to install nginx.

Each server's public address and the load balancer's address are Vultr reserved IPs, which you can see in the Vultr portal under **Network → Reserved IPs**, labelled `spinifex-vultr-demo:`. `tofu destroy` deletes them again.

---

## Watch a failover

On a three-node deployment, with the workbook above running:

1. Find which node runs a web server: `ssh` to node1 and run `spx get nodes`, which shows the VM count per node.
2. In the Vultr portal, **Stop** that node's VM.
3. Keep `curl`ing the server's public address.

In our measurement the instance was `running` on another node 2½ minutes after the stop, with its reserved IP moved to that node's VM, and answered HTTP once the guest had booted, 4¾ minutes after the stop. Start the stopped VM again and it rejoins without taking the instance back.

---

## Tear it down

Remove anything you built on the cluster first, then:

```bash
./deploy.sh destroy --name demo
```

This deletes the VMs and the VPC, plus two things Terraform never created and Vultr keeps billing: the VMs' boot volumes and any reserved IP labelled for the deployment. It fails, and lists what is left, if anything remains afterwards.

---

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `the API key was refused; check its IP allowlist` | Your machine's address is not in the key's Access Control list. Add it in the portal. |
| `never logged its Vultr allocator` | The node's address is not in the key's Access Control list. Add every node's address, shown in the Vultr portal under **Compute**, then run `up` again. |
| `plan vx1-g-16c-64g is not available in this region` | Pick another region with `--region`, or another plan of at least 16 vCPU with `--plan`. |
| `formation failed` | The full log is in `~/.spinifex/vultr/demo/form.log`. Run `up` again once the cause is fixed. |
| Any other failure | Logs are under `~/.spinifex/vultr/demo/`. `up` resumes from the failed step. |

---

## Limits

- **VMs only, one or three nodes.** Bare metal is not supported yet.
- **At least 16 vCPU per node.** Smaller VX plans report their vCPUs as hyperthreads and Spinifex schedules half of them.
- **The API key lives on the nodes**, at `/etc/spinifex/vultr/api-key`, readable only by the Spinifex daemon. It is never in the Terraform state or the instance metadata.
- **A deleted reserved IP keeps its quota slot for 24 hours**, by Vultr's documentation, so creating and deleting many addresses in a day can reach the account's limit.
- **Disassociating an Elastic IP leaves the instance with no public address.** AWS gives it a new one. Stopping and starting the instance gets it one.
- **An `apt upgrade` leaves Spinifex, OVS and OVN running.** The installer stops needrestart restarting them, so restart them yourself when it suits. The deploy also turns off unattended upgrades on these hosts.
