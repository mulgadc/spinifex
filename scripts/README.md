# Spinifex Scripts

Node installation, dev-environment lifecycle, guest image builds, and verification tooling for Spinifex.

## Install and node setup

| Script | What it does |
|---|---|
| `setup.sh` | The production binary installer, served from `install.mulgadc.com`. Installs deps, users, sudoers, files, systemd units, logrotate and udev rules. `SETUP_STAGES` runs a subset; `ISO_BUILD=1` runs it inside the ISO builder's chroot. |
| `setup-ovn.sh` | Bootstraps a compute node for OVN VPC networking: installs OVN/OVS, creates `br-int`, configures the WAN bridge (bridged, DHCP, or routed-NAT uplink), sets the chassis identity, and applies overlay sysctls. `--management` also starts the OVN central services. |
| `install-node.sh` | Forms a multi-node cluster across hosts that are already running Spinifex: rebuilds the OVN database in clustered form, runs `spx admin init` on the first host and `spx admin join` on the rest, then verifies. The scripted form of [install-multi-node](../docs/install/install-multi-node/README.md). Destroys the joining nodes' logical network state and crypto — see its `--help`. |
| `dev-install.sh` | Full local dev setup through the production installer: builds from source, packs a tarball, runs `setup.sh`, initialises the cluster, and starts services under systemd. |
| `clone-deps.sh` | Clones or updates the viperblock and predastore checkouts alongside spinifex for cross-repo development. |

## Dev environment

| Script | What it does |
|---|---|
| `reset-dev-env.sh` | Single-node reset: stops services, wipes `/etc/`, `/var/lib/`, `/var/log/` and `/run/spinifex`, rebuilds, reinstalls via `dev-install.sh`, and relaunches a smoke-test instance. Preserves the existing node/network topology from `spinifex.toml`. |
| `dev-env/*.conf` | Topology profiles for `reset-dev-env.sh --profile=NAME`, declaring which bridge carries the wan, lan and vpc planes: `single-nic` (all collapsed onto wan), `two-nic` (vpc folds onto lan), `three-plane` (a NIC each). |

## Verification and benchmarks

| Script | What it does |
|---|---|
| `smoke-test.sh` | End-to-end sanity check on a running node: imports an SSH keypair and the Ubuntu AMI, then launches an instance. `--create-vpc` builds the VPC, gateway, subnet and routes first and tears them down after; `--nodes N` launches through a spread placement group so each instance lands on a different physical server. `TEST_GPU=1` adds GPU passthrough checks. |
| `verify-network.sh` | Asserts a node's datapath matches its declared planes — EIP reachability, which plane carries Geneve, and which interfaces services bind. Catches what `smoke-test.sh` cannot, since none of its traffic leaves the box. |
| `test-ddil-phase1.sh` | systemd-level smoke test for daemon-local-autonomy tier 1. Destructive on the local node: stops and starts `spinifex-nats` and restarts `spinifex-daemon`, restoring both via a cleanup trap. |
| `check-coverage.sh` | Fails the build if total Go coverage falls below the minimum threshold, excluding packages exempt from the requirement. |
| `diff-coverage.sh` | Coverage check scoped to changed lines only, against an auto-detected base ref (`HEAD~1` on main, `origin/main` on dev, otherwise `origin/dev`). |
| `run-bench.sh` | Launches a benchmark against running Ubuntu instances, auto-detecting the AMI for the host architecture. Tracks nbdkit and perf alongside the run. |
| `disk-performance.sh` | Runs a `fio` random 70/30 read-write benchmark swept across 4k/16k/128k/1M. Writes JSON results to `/tmp/spinifex-disk-bench`; override with `BENCH_DIR`, `OUT_DIR`, `SIZE`, `JOBS`, `BLOCK_SIZES`. |
| `network-performance.sh` | iperf3 throughput from several clients to one server, driven over SSH. `--server-ip` sets the address clients dial, so pointing it at instance private addresses measures the VPC overlay. |
| `workload-performance.sh` | In-guest memory, disk, CPU and network workload test. Installs Go and runs the Badger test suite with jemalloc disabled and reduced parallelism. |

## Guest images

| Script | What it does |
|---|---|
| `build-system-image.sh` | Builds a minimal system AMI (Alpine or Ubuntu) from a manifest. All customisation happens inside the libguestfs appliance, so the build touches no host block device, no host mount, and needs no sudo. `--import` also registers it as an AMI. |
| `publish-system-image.sh` | Converts a built raw image to compressed qcow2, writes the `.sha256` sidecar the catalog verifier expects, and uploads both to the R2 bucket behind `iso.mulgadc.com/system-ami/`. |
| `build-microvm-image.sh` | Builds the minimal Alpine kernel + initramfs used by the QEMU microvm machine type and the lb-agent, carrying haproxy (ALB) and nginx-stream (NLB) data planes. |
| `mkosi-build.sh` | Runs an mkosi system-image build inside the pinned builder container, so a runner needs only Docker and the toolchain cannot drift. `--image` names an output and expands to the right profile list. |
| `mkosi-publish.sh` | Publishes an mkosi-built image to R2 from inside the same pinned container — qcow2 conversion, `.sha256` sidecar, and upload. |
| `mkosi-common.sh` | Shared plumbing sourced by both mkosi scripts. Builds the toolchain image once, with the builder account matched to the invoking user so bind-mounted output is not written back as root. |
| `mkosi-builder.Dockerfile` | The pinned image-build toolchain. Debian-hosted regardless of target distro, since mkosi bootstraps the target from its own archive. |

### `images/` — per-image build inputs

Each subdirectory holds a `manifest.conf`, init scripts, and a `setup.sh` that `build-system-image.sh` runs inside the libguestfs appliance after packages and files are placed. `*_test.sh` files are shell unit tests for the helper of the same name.

| Path | What it builds |
|---|---|
| `ecs-agent/` | The Alpine `spinifex-ecs-node` container-instance AMI (containerd + ecs-agent). `setup.sh` sets OpenRC exec bits, wires the CNI bin path, and applies the serial-console tweaks. |
| `ecs-agent-gpu/` | The Ubuntu ECS GPU container-instance AMI. Same CNI and containerd wiring adjusted for Ubuntu's package layout, plus the headless NVIDIA driver and nvidia-container-toolkit. |
| `eks-node/` | The unified Alpine EKS node AMI (K3s server *or* agent, selected at first boot). `setup.sh` installs the pinned, SHA-verified K3s binary and the role init scripts. |
| `eks-node-gpu/` | The Ubuntu EKS GPU worker AMI. Agent role only — GPU nodes never run the control plane — plus the NVIDIA GPU stack. |
| `rds-postgres/` | The `spinifex-rds-postgres` AMI. `setup.sh` sets exec bits on the OpenRC services and creates the root-only directory cloud-init drops the agent env file and gateway CA into. |
| `ubuntu-gpu-{amd,nvidia}-setup.sh` | Chroot setup for GPU-capable Ubuntu guest images: ROCm CLI tools and AMD firmware, or the DKMS-built headless NVIDIA driver. Guest-side only — host VFIO setup is `spx admin gpu setup`. |

### `images/eks-node/` — in-guest helpers

Installed into the EKS node AMI and run at boot or on a timer. All host communication goes through the AWS gateway over SigV4; the VM never speaks NATS directly.

| Script | What it does |
|---|---|
| `eks-node-role.sh` | First-boot role selector. Reads `SPINIFEX_K3S_ROLE` from the cloud-init env file and enables the matching services — nothing is enabled at bake time. |
| `k3s-prestart.sh` | Server prestart: refuses to start on a restore-block marker, derives `config.yaml` from its skeleton, stages the konnectivity-agent and CoreDNS manifests, and waits for the token-webhook kubeconfig. |
| `k3s-agent-prestart.sh` | Agent prestart shared by the OpenRC and systemd units. Requires `K3S_URL` and `K3S_TOKEN`, then resolves the kubelet provider-id from IMDSv2 for EKS parity. |
| `k3s-first-boot.sh` | Runs once after the server is healthy: reads the node-token and admin kubeconfig, rewrites the kubeconfig server address to the cluster NLB endpoint, and publishes both to the host. |
| `konnectivity-server-prestart.sh` | Waits for the K3s CA and admin kubeconfig, mints the konnectivity serving cert, and writes the apiserver replica count for the caller to source. |
| `mulga-eks-addon-sync.sh` | Pulls staged managed-addon descriptors through the gateway, renders each bundle into the K3s auto-deploy dir, and reports per-addon delivery status back. |
| `mulga-eks-etcd-snapshot.sh` | Snapshots K3s embedded etcd to predastore. Installed into two crond periodic dirs and picks its tier — `frequent` or `daily` — from the dir it was invoked from. |
| `mulga-eks-k3s-recovery.sh` | Applies a control-plane etcd-quorum recovery directive before k3s starts. Pull-based and a no-op unless the reconciler escalated a wedged control plane. |
| `mulga-eks-state-report.sh` | Publishes server health (`healthz`, node count) to the cluster reconciler. The apiserver binds the VPC node-ip, which the host daemon cannot probe directly. |
| `mulga-eks-provider-id.sh` | Writes the K3s config drop-in supplying the metadata the EBS CSI driver needs from IMDS: provider-id plus the region, zone and instance-type labels. |
| `mulga-ebs-byid.sh` | Mints the EBS-style `/dev/disk/by-id` symlink the upstream EBS CSI node plugin resolves, from the virtio-blk serial. Alpine's busybox mdev has no eudev to do it. |
| `mulga-mgmt-net.sh` | Boot oneshot bringing a multi-NIC system VM's interfaces up by MAC from the QEMU fw_cfg netcfg blob, before cloud-init's network stage. cloud-init cannot reliably pick the right NIC out of two. |
| `mulga-vpc-mtu.sh` | Boot oneshot pinning the VPC data NIC MTU so flannel sizes its VXLAN overlay to fit the OVN geneve underlay. Otherwise large frames are silently dropped while handshakes pass. |

## Launch helpers

| Script | What it does |
|---|---|
| `launch-gpu.sh` | Launches a GPU instance from the AMD or NVIDIA GPU AMI, selecting the AMI from the instance type's GPU brand. `--amd` / `--nvidia` override the detection. |
| `ecs-node-bringup.sh` | Launches an ECS container-instance VM. Renders the cloud-init user-data seeding the agent's control-plane config, IAM credentials and gateway CA — there is no ECS-specific launch API, by design. |

## `demo/` — GPU demo

| Script | What it does |
|---|---|
| `deploy.sh` | Deploys and starts the four-instance GPU demo (dashboard, YOLO, Ollama, qwen3-vl) from the `*_IP` and `VIDEO_PATH` env vars. |
| `dashboard_server.py` | Serves the demo web UI and proxies the streams from the GPU instances. |
| `yolo_stream.py` | Runs YOLO detection over a video file, serving an MJPEG stream of annotated frames and an SSE stream of qwen3-vl scene descriptions. |
