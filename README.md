<p align="center">
  <img src=".github/assets/banner.svg" alt="Spinifex: the open, AWS-compatible cloud you run yourself. EC2, EBS, S3, VPC, and IAM on your infrastructure, at the edge, or on a Neocloud." width="900">
</p>

<p align="center">
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.27+-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-AGPL--3.0-3fb950?style=flat-square" alt="License"></a>
  <a href="https://mulgadc.com"><img src="https://img.shields.io/badge/home-mulga-orange?style=flat-square&logo=data:image/svg%2bxml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIGZpbGw9IiNmZmYiIHZpZXdCb3g9IjAgMCAyNCAyNCI+PHBhdGggZD0iTTE2LjcxOCA4Ljg5MWMtMS4yODUgMS4zNy0zLjE3OCAyLjMxNy00LjY5NSAzLjQ0LS44NTQuNjMtMy4wODkgMi4yNy0xLjYxIDMuMzQ0IDEuNzg4IDEuMjk4IDYuMjQzLjE2NCA3Ljc4NS0xLjI2Ljg0LS43NzUuODE0LTEuODIyLS4zMjgtMi4yNTktMS4yMzUtLjQ3Mi0yLjY2LS4xMTEtMy45MTMuMDg3LS4wNDIuMDA3LS4xMzEuMDU1LS4xMjMtLjAyLjUyMy0uMzM2Ljk5My0uNzUgMS41MDMtMS4xMDIuNDkyLS4zNCAxLjA5Ny0uODI3IDEuNy0uODM4IDEuODcxLS4wMzQgMy43OTkuODkgNC4yODcgMi44MTUuODExIDMuMjAzLTMuMDA2IDUuNzE1LTUuNzg0IDUuOTQybDEuNjE0LS43NDdjLjYwNS0uMzYgMS4yMTctLjczNCAxLjc1Mi0xLjE5Ni4xMzMtLjExNS4yMy0uMjYzLjM0Mi0uMzczLjAyNy0uMDI2LjMwMi0uMTQ0LjE0NC0uMTUtMS40NDEuOTQ1LTMuMTI3IDEuNTcyLTQuODEzIDEuOTMyLS41OC4xMjMtMS4xOTUuMjQyLTEuNzg1LjI1Ny4wMTUuMDguMDg3LjA2NC4xNC4wOC40NzYuMTU0IDEuMDIuMjQ1IDEuNTE2LjMyLjA2OC4wNTItLjAwNi4wNzQtLjA2LjA4MS0uNDQ4LjA1OC0uOTIzLjE0My0xLjM3LjE2Ni0xLjI3LjA2NC0yLjU1LS4wNjgtMy43NzctLjM4bC0uNjktMS45NTdjLS4wNi4wMDYtLjA2My4wNi0uMDc5LjEwMy0uMDYuMTctLjMwNSAxLjQxNy0uMzg3IDEuNDM1LS42Ni0uMi0xLjI1MS0uNjQ3LTEuNzM2LTEuMTMybDEuMTUtMi4wNi0xLjgzNSAxLjAxMWMtLjI1OC0uNTgzLS4zMDUtMS4yNDMtLjIxOS0xLjg3bDIuMzc1LTEuMThjLS43MzktLjA0OS0xLjQ4Ni4wNjgtMi4yMi4xNDUuMTI1LS4zNi4yNzctLjcxOC40NjItMS4wNTEuMDY3LS4xMjIuNDI2LS43MjEuNTItLjcyNC44Ni4xNjQgMS43ODYuMjQxIDIuNjQ4LjA3NGwtMS44Ny0uNzcxLS4wNjktLjA5M2MuMzgyLS4zNi43Ny0uNzE3IDEuMTk5LTEuMDI0LjgwNC4yMzQgMS42My40MjIgMi40NzMuNDMzTDkuMzUgOS4zNDJhNyA3IDAgMCAxIC41NjctLjQwMWMuMTYtLjEwMy42MDktLjQwMi43NzItLjM4LjY1LjIyMSAxLjMyNC4zNzggMi4wMS40MzMtLjMxMy0uMzI1LS45MDktLjU2NC0xLjIxLS44NjgtLjAzMy0uMDM0LS4wNTgtLjAzNi0uMDQzLS4wOTguNzg3LS40NTIgMS42NjItLjkwMSAyLjM0LTEuNTE3LjY5LS42MjkgMS40MjEtMS42NTYuOTI0LTIuNjA1LjU3My4xODIgMS4wNjcuOTA2IDEuMDU0IDEuNTEyLS4wMzQgMS42NzYtMS44MjIgMy4xNy0yLjk0MyA0LjIyMiAxLjczMi0uNzI4IDMuNzE0LTIuMjMgMy43MS00LjMwNS0uMDAzLTEuNjg4LTEuNTQtMi4zNjUtMi45OTMtMi41MThhMy4yIDMuMiAwIDAgMS0uMzg1IDEuMDg3Yy0uNDI4LjcxOS0xLjMwMiAxLjE2OC0xLjc1OCAxLjkxNS0uMzExLjUxLS4zNyAxLjE5NS0xLjAzMSAxLjM5LS4yNy4wOC0uNjE3LjA5My0uODk3LjA5NGwuNjE1LS4zMjVjLjY4OS0uNTA2LjY3Ny0xLjQyIDEuMDk5LTIuMS0uMDUtLjA3LS41NDUtLjItLjY2LS4yMjktLjUxLS4xMjUtMS4zNzUtLjI4NS0xLjg4Ni0uMjUtLjE1Ny4wMS0uODY5LjEyMS0uOTMuMjQyLS4wODguMTc3LjM0MS45My40NjggMS4xMDEuMDI1LjAzNS4wNzcuMDI2LjA4LjAzLjAyNC4wMzEuMDA3LjEwNS0uMDYuMDhhNSA1IDAgMCAxLS40MDQtLjI4M2MtLjUwMy0uMzg4LTEuMDc4LS45NzQtMS40OTktMS40NDctLjA2OC0uMTY2LS4xNi0uMzUuMDAyLS40ODcuODg4LS41NSAxLjc2OC0xLjEyNiAyLjY3LTEuNjUxIDIuNjcxLTEuNTU4IDUuNzExLTMuMzE4IDguMjY3LS40NjUgMi4xMTkgMi4zNjYgMS41MTQgNS4yMTItLjUxMiA3LjM3MnptLTguMjI0LTUuMzhjLjY5OS0uMTIyIDIuMDE4LjU3MiAyLjIzNS0uNDU2LjAyMS0uMS0uMDM2LS4xNTcuMDItLjI1NS4wNDMtLjA3My4yODYtLjI1LjM3LS4zMTguMjctLjIxNy41NzctLjM4Ny44NDItLjYxMi0xLjAyOS4xNjYtMi4wNjUuNzU2LTIuOTY0IDEuMjc3LS4xNDkuMDg2LS4zMS4xNzYtLjQ1MS4yNzMtLjAzNy4wMjUtLjA3NS0uMDA2LS4wNTMuMDltLTQuODMgMTAuODQ0Yy0xLjcxNSAxLjg4MS0xLjMyNSA0LjU3LjQ5NCA2LjIxNyAxLjgxMSAxLjY0MSA0LjY2IDIuMjIzIDcuMDQ4IDIuMTA4IDIuMTUyLS4xMDMgNC4zMzctLjgxMiA2LjQ2LS4wOTEuODI1LjI4IDEuNTQ2LjgwNSAyLjE2IDEuNDExLS4wODMtLjQtLjMyNC0uODE5LS41NTgtMS4xNTktMi4xMy0zLjA5NS02LjI3LTEuOTM1LTkuNDI2LTIuNTUzLTEuNzExLS4zMzUtMy40OTEtMS4xMTgtNC41MzMtMi41NjctLjkwMS0xLjI1My0xLjA0Mi0yLjczLS41OTUtNC4xOTQtLjA5MS0uMDktLjk1Mi43MjEtMS4wNS44MjhtNi4zNjYtOC42NjdjLS4zODIuMzM5LS43ODcuNjYtMS4yMTIuOTQ4bC0xLjIwNy41OWMxLjAyMi4wMzkgMi4wOC0uNTQ2IDIuNDItMS41MzciLz48L3N2Zz4=" alt="mulgadc.com"></a>
</p>

<p align="center">
  <a href="#what-is-spinifex">What is Spinifex?</a> ·
  <a href="#the-platform">Platform</a> ·
  <a href="#three-ways-to-deploy">Deploy</a> ·
  <a href="#aws-compatibility">AWS Compatibility</a> ·
  <a href="#core-components">Components</a> ·
  <a href="#architecture-at-a-glance">Architecture</a> ·
  <a href="#self-hosted-installation">Installation</a> ·
  <a href="https://docs.mulgadc.com">Docs</a>
</p>

---

# Spinifex: the open, AWS-compatible cloud you run yourself

Spinifex, built by [Mulga](https://mulgadc.com), recreates the AWS services your software already uses (EC2, EBS, S3, VPC, IAM) on infrastructure you control. Move workloads off the hyperscalers, run AI at a fraction of the cost, or take the cloud to the edge, without rewriting a line. The exact build you ship to AWS, Spinifex serves on your own hardware: same APIs, same Terraform, same SDKs.

## What is Spinifex?

Spinifex is an open-source infrastructure platform that recreates the AWS service surface (EC2, EBS, S3, VPC, IAM) on bare-metal, edge, and on-premises environments. Run cloud-native software without a hyperscaler.

Built for teams that need:

- The AWS tooling you already use (AWS CLI, SDKs, Terraform), with nothing to rewrite
- Full control of the stack: your hardware, your network, your data, your keys
- A cloud that keeps running offline, through disconnection, with no external control plane
- An open core (AGPL-3.0), auditable and yours to keep, with no lock-in

You change *where* your software runs, not *what* it is.

## The Platform

<p align="center">
  <img src=".github/assets/platform.svg" alt="The Spinifex stack: your apps and AWS tooling on top, the Spinifex AWS-compatible service layer, running on standard Linux on CPU and GPU hardware, deployable to Neocloud, on-premise, or the edge." width="900">
</p>

From commodity hardware up to unmodified AWS tooling, every layer is replaceable and yours to own.

## Three Ways To Deploy

The same AWS-compatible platform runs in three shapes. Pick the one that matches your hardware.

### Neocloud

Lift and shift onto a partner Neocloud without rewriting workloads. GPU capacity (H100 / H200 / B200), cheaper, available now. Change an endpoint, keep the software.

### On-premise

Host Spinifex on your own hardware in your own data centre. You get a multi-node HA cluster integrated with your storage and networking, so the stack, the data, and the jurisdiction stay yours.

### Edge

Cloud where the cloud can't reach. Air-gapped sites, vehicles, vessels, factories, and clinics. Compute next to the data, running through disconnection, on hardware you own. The same AWS APIs your software already uses.

## AWS Compatibility

Spinifex speaks the AWS API surface. The AWS SDKs, AWS CLI, and Terraform all work unchanged, whether you deploy at the edge, on-premise, or on a partner Neocloud.

| Service | What it is | Status |
| --- | --- | --- |
| **EC2** | Compute | Available |
| **EBS** | Block Storage | Available |
| **S3** | Object Storage | Available |
| **VPC** | Networking | Available |
| **IAM** | Identity & Auth | Available |
| **ALB / NLB** | Load Balancers | Available |
| **EKS** | Kubernetes | Available |
| **ECR** | Container Registry | Available |
| **ECS** | Container Service | Available |
| **RDS** | Databases | Available |
| **Bedrock** | AI Deployment | Q3 2026 |

Roadmap items ship under the same AWS API surface. Code written for AWS today keeps working the moment they land. [Track what's shipped in the release notes](https://github.com/mulgadc/spinifex/releases).

## Core Components

### Spinifex (Compute and Platform Services – EC2, EKS, ECS, RDS)

Spinifex is a minimal VM orchestration layer built on top of QEMU, exposing an EC2 compatible API. It manages lifecycle operations like start, stop, and terminate through QEMU's QMP interface. Designed to be straightforward and scriptable, Spinifex lets you launch VMs with the AWS CLI, SDKs, or Terraform, without Kubernetes or a heavyweight orchestrator.

The managed services build on that same instance layer. An EKS node group, an ECS container instance, and an RDS database are Spinifex VMs on your own hardware, each driven through its own AWS API.

- **EC2** – VM lifecycle on bare metal, cloud-init and IMDS metadata, key pairs, tags, spot instances, capacity reservations, and GPU passthrough
- **EKS** – Managed Kubernetes on k3s, with HA control planes, managed node groups, cluster add-ons, an OIDC provider, and IAM access entries
- **ECS** – Task and service scheduling across container instances, with awsvpc ENIs, capacity providers, rolling deployments, and GPU-aware placement
- **ECR** – Private container registry backed by Predastore, with content-addressed blobs deduplicated per account and lifecycle policies
- **RDS** – Managed PostgreSQL and MariaDB, with parameter groups, automated backups, and DNS endpoints served by Northstar
- **ALB / NLB** – Layer 7 load balancing on HAProxy and layer 4 on nginx `stream`, with health checks and ACM-issued listener certificates
- **VPC and IAM** – OVN-backed networking and security groups, with roles, policies, and SigV4 auth applied across every service

### Viperblock (Block Storage – EBS Alternative)

[Viperblock](https://github.com/mulgadc/viperblock) is a high-performance, WAL-backed block storage service that replicates volumes across multiple nodes. It's built for reliability and speed, with support for snapshots, recovery, and direct connection to QEMU instances using NBD or virtio-blk.

- Fast, durable virtual disks
- Replication for resilience
- Exposed over NBD or embedded in VMs
- High performance WAL logs on local NVMe drives, reducing IO traffic to S3
- In-memory read/write block cache

### Predastore (Object Storage – S3-Compatible)

[Predastore](https://github.com/mulgadc/predastore) is a fully S3-compatible object storage system. It supports the AWS S3 API, including Signature V4 authentication, multipart uploads, and Terraform provisioning. Predastore chunks data and distributes it across nodes using Reed-Solomon erasure coding, so it survives node loss and suits large-scale or low-bandwidth sites.

- S3-compatible API and auth
- Multipart uploads, streaming reads/writes
- Data redundancy with Reed-Solomon encoding

### Northstar (Authoritative DNS)

[Northstar](https://github.com/mulgadc/northstar) is a lightweight authoritative DNS server that handles both internal service discovery and public-facing DNS for a Spinifex cluster. Zones are human-readable TOML files, served from the local filesystem or synced from Predastore, keeping DNS configuration in the same object storage as the rest of your infrastructure.

- Authoritative DNS over UDP, TCP, and DNS-over-TLS
- Service discovery for cluster components via SRV records
- Zone files stored locally or in S3-compatible storage

## Architecture at a Glance

<p align="center">
  <img src=".github/assets/architecture.svg" alt="Spinifex message-driven architecture: standard AWS clients call the AWS Gateway over HTTPS with SigV4, which publishes to a NATS bus answered by stateless ec2/ebs/s3/vpc/iam daemons over local backends." width="900">
</p>

Every AWS API call is authenticated at the gateway, published to a NATS subject, and answered by whichever daemon claims it. Daemons are stateless: scale horizontally by starting more. No etcd, no Kubernetes, no external control plane. Just systemd units, a NATS cluster, and your hardware. Deep dive: **[`docs/DESIGN.md`](docs/DESIGN.md)**.

## Key Features

- **AWS-compatible APIs.** Use the AWS CLI, SDKs, and Terraform you already know. Repoint your endpoint and ship; nothing to rewrite.
- **Zero cloud dependency.** Runs entirely on your hardware. No phone-home, no control plane, no external authority. Works fully offline.
- **Bare-metal compute.** QEMU-based with direct hardware access. No hypervisor tax, no abstraction overhead.
- **Built-in storage.** Block and object storage included: NVMe caching, Reed-Solomon erasure coding, and replication out of the box.
- **Edge-first architecture.** Designed for disconnected, contested, and resource-constrained environments from day one.
- **Open core, no lock-in.** AGPL-3.0 with a commercial option. Inspect it, modify it, deploy it. The platform is yours to keep.

## Live Demo

Create a live sandbox in under a minute. The fastest way to experience the stack. Try Spinifex against one of your own workloads on infrastructure we run for you.

* Live in under a minute. No card, no install.
* Yours for 72 hours to test and experiment.
* Real AWS-compatible endpoint, access keys and console.
* EC2, EBS, S3, VPC, IAM, EKS and RDS, ready to call.
* Point your existing AWS CLI or Terraform straight at it.

Sign up at [https://mulgadc.com/signup](https://mulgadc.com/signup)

## Self-hosted Installation

Installation requires an Ubuntu 26.04 or Debian 13 system. See the detailed documentation at [docs.mulgadc.com](https://docs.mulgadc.com) for installing Spinifex.

### Bare Metal ISO

The recommended installation is a [bootable x86 installer](https://iso.mulgadc.com/spinifex.iso) for bare-metal hardware.

```bash
curl -fLO https://iso.mulgadc.com/spinifex.iso
```

Follow the [USB install guide](https://docs.mulgadc.com/docs/install-usb) to write the ISO to USB and install on your hardware.

### Single Node Install

>*Prerequisite:* Spinifex requires a Linux bridge configured on the host for VM networking. See the [single-node install guide](https://docs.mulgadc.com/docs/install#prerequisites) for setup details.

```bash
curl -fsSL https://install.mulgadc.com | bash

sudo /usr/local/share/spinifex/setup-ovn.sh --management

sudo spx admin init --node node1 --nodes 1

sudo systemctl start spinifex.target

export AWS_PROFILE=spinifex

aws ec2 describe-instance-types
```

### Development Setup

For a complete development environment see the [Source Install](https://docs.mulgadc.com/docs/install-source) documentation.

### Component Repositories

Spinifex coordinates these independent components:

- **[Predastore](https://github.com/mulgadc/predastore)** - S3-compatible object storage
- **[Viperblock](https://github.com/mulgadc/viperblock)** - EBS-compatible block storage
- **[Northstar](https://github.com/mulgadc/northstar)** - Authoritative DNS server

Each component can be developed independently. See component-specific documentation for focused development guides.

## Spinifex UI

Spinifex ships with a built-in web console, an optional alternative to the AWS CLI, SDKs, and Terraform. If you know the AWS Management Console, the Spinifex UI fills the same role: a browser-based view of your instances, volumes, buckets, VPCs, and IAM resources, without leaving your own network.

<p align="center">
  <img src=".github/assets/spinifex-ui.jpg" alt="Spinifex web console dashboard" width="900">
</p>

Each node serves the console on port `3000` over TLS, and it comes up with `spinifex.target`:

- **Same API, different surface.** The UI issues the same AWS SigV4 calls the CLI does, so RBAC, audit trails, and IAM policies apply the same way.
- **Single sign-on against your AWS credentials.** Log in with the access keys from `~/.aws/credentials` on the node where Spinifex is installed. There is no separate user database.
- **Self-hosted, works offline.** The Spinifex binary embeds the UI and each node serves it directly, with no external CDN or analytics calls.

For the full walkthrough, covering first-time TLS certificate trust, login, and a feature tour, see [**Launching the Web UI**](https://docs.mulgadc.com/docs/setting-up-your-cluster#7-launching-the-web-ui) in the cluster setup guide.

## Development Philosophy

### Built by Engineers, For Engineers

Spinifex is developed by experienced infrastructure engineers with deep AWS expertise, including former AWS team members who understand the intricacies of building production-grade cloud services. Our team brings decades of combined experience from AWS, enterprise infrastructure, and edge computing environments.

**Real-World Experience:**

- Production AWS service development and operations
- Large-scale infrastructure deployment and management
- Edge computing and resource-constrained environments
- Enterprise security and compliance requirements

### AI-Assisted Development

While Spinifex is architected and implemented by experienced engineers, we leverage **Claude Code** (Anthropic's AI coding assistant) to accelerate certain development tasks. This approach combines human expertise with AI efficiency:

**How We Use Claude Code:**

- **Code Generation**: Boilerplate AWS API structures and handlers
- **Documentation**: Comprehensive development guides and API documentation
- **Testing**: Test case generation and validation scenarios
- **Refactoring**: Large-scale code restructuring and optimization

**What Remains Human-Driven:**

- **Architecture Decisions**: Core system design and scalability choices
- **Security Implementation**: Authentication, encryption, and threat modeling
- **Performance Optimization**: Real-world performance tuning and benchmarking
- **Production Operations**: Deployment strategies and operational procedures

This hybrid approach ensures Spinifex benefits from both proven engineering expertise and modern development acceleration, while maintaining the quality and reliability standards required for production infrastructure.

## Trademarks

"AWS", "Amazon Web Services", and all related service names (EC2, EBS, S3, VPC, IAM, and others) are trademarks of Amazon.com, Inc. or its affiliates. Mulga Defense Corporation and Spinifex are independent and not affiliated with, endorsed by, or sponsored by Amazon.com, Inc. or Amazon Web Services, Inc. References to AWS services describe interoperability and compatibility only.

## License

Spinifex is open source under the [GNU Affero General Public License v3.0](LICENSE). You're free to use, modify, and deploy it anywhere you need reliable infrastructure without depending on centralized cloud platforms.
