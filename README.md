<p align="center">
  <img src=".github/assets/banner.svg" alt="Spinifex: the open, AWS-compatible cloud you run yourself. EC2, EBS, S3, VPC, and IAM on your infrastructure, at the edge, or on a Neocloud." width="900">
</p>

<p align="center">
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.27+-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go"></a>
  <a href="https://pkg.go.dev/github.com/mulgadc/spinifex"><img src="https://img.shields.io/badge/pkg.go.dev-Reference-007D9C?style=flat-square&logo=go&logoColor=white" alt="Go Reference"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-AGPL--3.0-3fb950?style=flat-square" alt="License"></a>
  <a href="https://mulgadc.com"><img src="https://img.shields.io/badge/Site-mulga-orange?style=flat-square&logo=data:image/svg%2bxml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIGZpbGw9IiNmZmYiIHZpZXdCb3g9IjAgMCAyNCAyNCI+PHBhdGggZD0iTTE2LjcxOCA4Ljg5MWMtMS4yODUgMS4zNy0zLjE3OCAyLjMxNy00LjY5NSAzLjQ0LS44NTQuNjMtMy4wODkgMi4yNy0xLjYxIDMuMzQ0IDEuNzg4IDEuMjk4IDYuMjQzLjE2NCA3Ljc4NS0xLjI2Ljg0LS43NzUuODE0LTEuODIyLS4zMjgtMi4yNTktMS4yMzUtLjQ3Mi0yLjY2LS4xMTEtMy45MTMuMDg3LS4wNDIuMDA3LS4xMzEuMDU1LS4xMjMtLjAyLjUyMy0uMzM2Ljk5My0uNzUgMS41MDMtMS4xMDIuNDkyLS4zNCAxLjA5Ny0uODI3IDEuNy0uODM4IDEuODcxLS4wMzQgMy43OTkuODkgNC4yODcgMi44MTUuODExIDMuMjAzLTMuMDA2IDUuNzE1LTUuNzg0IDUuOTQybDEuNjE0LS43NDdjLjYwNS0uMzYgMS4yMTctLjczNCAxLjc1Mi0xLjE5Ni4xMzMtLjExNS4yMy0uMjYzLjM0Mi0uMzczLjAyNy0uMDI2LjMwMi0uMTQ0LjE0NC0uMTUtMS40NDEuOTQ1LTMuMTI3IDEuNTcyLTQuODEzIDEuOTMyLS41OC4xMjMtMS4xOTUuMjQyLTEuNzg1LjI1Ny4wMTUuMDguMDg3LjA2NC4xNC4wOC40NzYuMTU0IDEuMDIuMjQ1IDEuNTE2LjMyLjA2OC4wNTItLjAwNi4wNzQtLjA2LjA4MS0uNDQ4LjA1OC0uOTIzLjE0My0xLjM3LjE2Ni0xLjI3LjA2NC0yLjU1LS4wNjgtMy43NzctLjM4bC0uNjktMS45NTdjLS4wNi4wMDYtLjA2My4wNi0uMDc5LjEwMy0uMDYuMTctLjMwNSAxLjQxNy0uMzg3IDEuNDM1LS42Ni0uMi0xLjI1MS0uNjQ3LTEuNzM2LTEuMTMybDEuMTUtMi4wNi0xLjgzNSAxLjAxMWMtLjI1OC0uNTgzLS4zMDUtMS4yNDMtLjIxOS0xLjg3bDIuMzc1LTEuMThjLS43MzktLjA0OS0xLjQ4Ni4wNjgtMi4yMi4xNDUuMTI1LS4zNi4yNzctLjcxOC40NjItMS4wNTEuMDY3LS4xMjIuNDI2LS43MjEuNTItLjcyNC44Ni4xNjQgMS43ODYuMjQxIDIuNjQ4LjA3NGwtMS44Ny0uNzcxLS4wNjktLjA5M2MuMzgyLS4zNi43Ny0uNzE3IDEuMTk5LTEuMDI0LjgwNC4yMzQgMS42My40MjIgMi40NzMuNDMzTDkuMzUgOS4zNDJhNyA3IDAgMCAxIC41NjctLjQwMWMuMTYtLjEwMy42MDktLjQwMi43NzItLjM4LjY1LjIyMSAxLjMyNC4zNzggMi4wMS40MzMtLjMxMy0uMzI1LS45MDktLjU2NC0xLjIxLS44NjgtLjAzMy0uMDM0LS4wNTgtLjAzNi0uMDQzLS4wOTguNzg3LS40NTIgMS42NjItLjkwMSAyLjM0LTEuNTE3LjY5LS42MjkgMS40MjEtMS42NTYuOTI0LTIuNjA1LjU3My4xODIgMS4wNjcuOTA2IDEuMDU0IDEuNTEyLS4wMzQgMS42NzYtMS44MjIgMy4xNy0yLjk0MyA0LjIyMiAxLjczMi0uNzI4IDMuNzE0LTIuMjMgMy43MS00LjMwNS0uMDAzLTEuNjg4LTEuNTQtMi4zNjUtMi45OTMtMi41MThhMy4yIDMuMiAwIDAgMS0uMzg1IDEuMDg3Yy0uNDI4LjcxOS0xLjMwMiAxLjE2OC0xLjc1OCAxLjkxNS0uMzExLjUxLS4zNyAxLjE5NS0xLjAzMSAxLjM5LS4yNy4wOC0uNjE3LjA5My0uODk3LjA5NGwuNjE1LS4zMjVjLjY4OS0uNTA2LjY3Ny0xLjQyIDEuMDk5LTIuMS0uMDUtLjA3LS41NDUtLjItLjY2LS4yMjktLjUxLS4xMjUtMS4zNzUtLjI4NS0xLjg4Ni0uMjUtLjE1Ny4wMS0uODY5LjEyMS0uOTMuMjQyLS4wODguMTc3LjM0MS45My40NjggMS4xMDEuMDI1LjAzNS4wNzcuMDI2LjA4LjAzLjAyNC4wMzEuMDA3LjEwNS0uMDYuMDhhNSA1IDAgMCAxLS40MDQtLjI4M2MtLjUwMy0uMzg4LTEuMDc4LS45NzQtMS40OTktMS40NDctLjA2OC0uMTY2LS4xNi0uMzUuMDAyLS40ODcuODg4LS41NSAxLjc2OC0xLjEyNiAyLjY3LTEuNjUxIDIuNjcxLTEuNTU4IDUuNzExLTMuMzE4IDguMjY3LS40NjUgMi4xMTkgMi4zNjYgMS41MTQgNS4yMTItLjUxMiA3LjM3MnptLTguMjI0LTUuMzhjLjY5OS0uMTIyIDIuMDE4LjU3MiAyLjIzNS0uNDU2LjAyMS0uMS0uMDM2LS4xNTcuMDItLjI1NS4wNDMtLjA3My4yODYtLjI1LjM3LS4zMTguMjctLjIxNy41NzctLjM4Ny44NDItLjYxMi0xLjAyOS4xNjYtMi4wNjUuNzU2LTIuOTY0IDEuMjc3LS4xNDkuMDg2LS4zMS4xNzYtLjQ1MS4yNzMtLjAzNy4wMjUtLjA3NS0uMDA2LS4wNTMuMDltLTQuODMgMTAuODQ0Yy0xLjcxNSAxLjg4MS0xLjMyNSA0LjU3LjQ5NCA2LjIxNyAxLjgxMSAxLjY0MSA0LjY2IDIuMjIzIDcuMDQ4IDIuMTA4IDIuMTUyLS4xMDMgNC4zMzctLjgxMiA2LjQ2LS4wOTEuODI1LjI4IDEuNTQ2LjgwNSAyLjE2IDEuNDExLS4wODMtLjQtLjMyNC0uODE5LS41NTgtMS4xNTktMi4xMy0zLjA5NS02LjI3LTEuOTM1LTkuNDI2LTIuNTUzLTEuNzExLS4zMzUtMy40OTEtMS4xMTgtNC41MzMtMi41NjctLjkwMS0xLjI1My0xLjA0Mi0yLjczLS41OTUtNC4xOTQtLjA5MS0uMDktLjk1Mi43MjEtMS4wNS44MjhtNi4zNjYtOC42NjdjLS4zODIuMzM5LS43ODcuNjYtMS4yMTIuOTQ4bC0xLjIwNy41OWMxLjAyMi4wMzkgMi4wOC0uNTQ2IDIuNDItMS41MzciLz48L3N2Zz4=" alt="mulgadc.com"></a>
  <a href="https://docs.mulgadc.com"><img src="https://img.shields.io/badge/Docs-spinifex-0969da?style=flat-square&logo=data:image/svg%2bxml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIGZpbGw9IiNmZmYiIHZpZXdCb3g9IjAgMCAyNCAyNCI+PHBhdGggZD0iTTYuNSAySDIwYTEgMSAwIDAgMSAxIDF2MTQuNWExIDEgMCAwIDEtMSAxSDYuNWExLjUgMS41IDAgMCAwIDAgM0gyMWExIDEgMCAwIDEgMCAySDYuNUEzLjUgMy41IDAgMCAxIDMgMjBWNS41QTMuNSAzLjUgMCAwIDEgNi41IDJ6bTEgNGExIDEgMCAwIDAgMCAyaDlhMSAxIDAgMCAwIDAtMmgtOXptMCA0YTEgMSAwIDAgMCAwIDJoOWExIDEgMCAwIDAgMC0yaC05eiIvPjwvc3ZnPg==" alt="docs.mulgadc.com"></a>
</p>


<p align="center">
  <a href="#try-it-live">Live Demo</a> ·
  <a href="#services">Services</a> ·
  <a href="#deploy-anywhere">Deploy</a> ·
  <a href="#install">Install</a> ·
  <a href="#architecture">Architecture</a>
</p>

---

# Spinifex: the open, AWS-compatible cloud you run yourself

Spinifex, built by [Mulga](https://mulgadc.com), recreates the AWS services your software already uses on infrastructure you control. Move workloads off the hyperscalers, run AI at a fraction of the cost, or take the cloud to the edge, without rewriting a line.

- **Your AWS tooling, unchanged.** The AWS CLI, SDKs, and Terraform work as they are.
- **Full control of the stack.** Your hardware, data, and keys, with no external control plane, running through disconnection.
- **Open core under AGPL-3.0.** Auditable, yours to keep, with no lock-in.

You change *where* your software runs, not *what* it is.

## Try It Live

Get a real Spinifex endpoint in under a minute, with no card and no install. The sandbox is yours for 72 hours, with access keys, the web console, and EC2, EBS, S3, VPC, IAM, EKS, and RDS ready to call. Point your existing AWS CLI or Terraform straight at it.

**[Create your sandbox →](https://mulgadc.com/signup)**

## Same Commands, Your Cloud

```bash
export AWS_PROFILE=spinifex
aws ec2 describe-images
aws ec2 run-instances --image-id ami-xxxxxxxx --instance-type t3.micro
aws s3 mb s3://my-bucket
```

For Terraform, start from the [example workbooks](https://docs.mulgadc.com/docs/nginx-webserver).

## Why Not LocalStack or Floci?

[LocalStack](https://github.com/localstack/localstack) and [Floci](https://github.com/floci-io/floci) are emulators for local development and CI testing. An emulator returns a plausible response; Spinifex boots the machine. A call launches a real KVM guest, attaches a replicated Viperblock volume, or writes an erasure-coded object into Predastore, so Spinifex can run your production workloads on hardware you own. Test against an emulator, then run in production on Spinifex.

## Services

The platform serves **392 AWS API operations** across the services below. See exactly which ones in the [coverage report](https://docs.mulgadc.com/coverage).

| Service | What it is | Built on | Status |
| --- | --- | --- | --- |
| **EC2** | Compute | QEMU/KVM, with spot instances and GPU passthrough | Available |
| **EBS** | Block storage | [Viperblock](https://github.com/mulgadc/viperblock): replicated, with a WAL on local NVMe | Available |
| **S3** | Object storage | [Predastore](https://github.com/mulgadc/predastore): Reed-Solomon erasure coding | Available |
| **VPC** | Networking | OVN, with security groups | Available |
| **IAM** | Identity & auth | SigV4 across every service | Available |
| **ALB / NLB** | Load balancers | HAProxy (L7), nginx `stream` (L4), with ACM certificates | Available |
| **EKS** | Kubernetes | k3s, with HA control planes and managed node groups | Available |
| **ECR** | Container registry | Predastore, deduplicated per account | Available |
| **ECS** | Container service | Spinifex instances | Available |
| **RDS** | Databases | PostgreSQL and MariaDB, with automated backups | Available |
| **Bedrock** | AI deployment | | Q4 2026 |

Roadmap services ship under the same AWS API surface, so code written for AWS today works the moment they land. [Track what's shipped in the release notes](https://github.com/mulgadc/spinifex/releases).

## Deploy Anywhere

<p align="center">
  <img src=".github/assets/platform.svg" alt="The Spinifex stack: your apps and AWS tooling on top, the Spinifex AWS-compatible service layer, running on standard Linux on CPU and GPU hardware, deployable to Neocloud, on-premise, or the edge." width="900">
</p>

- **Neocloud:** lift and shift onto partner GPU capacity (H100 / H200 / B200). Change an endpoint, keep the software.
- **On-premise:** a multi-node HA cluster in your own data centre, so the stack, the data, and the jurisdiction stay yours.
- **Edge:** air-gapped sites, vehicles, vessels, factories, and clinics, with compute next to the data.

## Install

Spinifex runs on Ubuntu 26.04 or Debian 13. The full guides are at [docs.mulgadc.com](https://docs.mulgadc.com).

>*Prerequisite:* Spinifex requires a Linux bridge configured on the host for VM networking. See the [single-node install guide](https://docs.mulgadc.com/docs/install#prerequisites) for setup details.

**Bare metal (recommended):** download the [bootable x86 installer](https://iso.mulgadc.com/spinifex.iso) and follow the [USB install guide](https://docs.mulgadc.com/docs/install-usb).

```bash
curl -fLO https://iso.mulgadc.com/spinifex.iso
```

**Single node:**

```bash
curl -fsSL https://install.mulgadc.com | bash
sudo /usr/local/share/spinifex/setup-ovn.sh --management
sudo spx admin init --node node1 --nodes 1
sudo systemctl start spinifex.target
export AWS_PROFILE=spinifex
aws ec2 describe-instance-types
```

**From source:** see the [Source Install](https://docs.mulgadc.com/docs/install-source) guide.

## Web Console

<p align="center">
  <img src=".github/assets/spinifex-ui.jpg" alt="Spinifex web console dashboard" width="900">
</p>

Spinifex ships with a built-in web console for your instances, volumes, buckets, networks, and IAM. See [Launching the Web UI](https://docs.mulgadc.com/docs/setting-up-your-cluster#7-launching-the-web-ui).

## Architecture

<p align="center">
  <img src=".github/assets/architecture.svg" alt="Spinifex message-driven architecture: standard AWS clients call the AWS Gateway over HTTPS with SigV4, which publishes to a NATS bus answered by stateless ec2/ebs/s3/vpc/iam daemons over local backends." width="900">
</p>

Every AWS API call is authenticated at the gateway, published to a NATS subject, and answered by whichever daemon claims it. Daemons are stateless, so you scale horizontally by starting more. Deep dive: **[`docs/DESIGN.md`](docs/DESIGN.md)**.

## How We Build Spinifex

Spinifex is built by infrastructure engineers with deep AWS experience, including former AWS team members, drawing on decades of combined work across AWS, enterprise infrastructure, and edge computing. Engineers architect and implement it, with **Claude Code** accelerating parts of the work.

| Claude Code assists with | Engineers own |
| --- | --- |
| Boilerplate AWS API structures and handlers | Architecture and scalability decisions |
| Development guides and API documentation | Authentication, encryption, and threat modelling |
| Test case generation and validation scenarios | Real-world performance tuning and benchmarking |
| Large-scale restructuring and refactoring | Deployment strategy and production operations |

## Trademarks

"AWS", "Amazon Web Services", and all related service names (EC2, EBS, S3, VPC, IAM, and others) are trademarks of Amazon.com, Inc. or its affiliates. Mulga Defense Corporation and Spinifex are independent and not affiliated with, endorsed by, or sponsored by Amazon.com, Inc. or Amazon Web Services, Inc. References to AWS services describe interoperability and compatibility only.

## License

Spinifex is open source under the [GNU Affero General Public License v3.0](LICENSE), with a commercial license available.
