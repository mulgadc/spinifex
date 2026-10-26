# Spinifex Scripts

Node installation, dev-environment lifecycle, guest image builds, and verification tooling for Spinifex.

## Install and node setup

| Script | What it does |
|---|---|
| `setup.sh` | The production binary installer, served from `install.mulgadc.com`. Installs deps, users, sudoers, the host firewall policy, files, systemd units, logrotate and udev rules, and loads `openvswitch`, `geneve`, `vhost_net` and `tun`, listing them in `/etc/modules-load.d/spinifex.conf` for later boots. It masks firewalld wherever it is installed, since the Spinifex nftables policy is the only supported node firewall. An unset `--firewall` keeps the mode recorded in `/etc/spinifex/firewall/mode`, and otherwise arms the policy when firewalld was active. Where systemd-resolved is installed but not running, resolvconf is absent and NetworkManager runs, it hands host DNS to resolved: it saves `/etc/resolv.conf` once to `/var/lib/spinifex-reset/resolv.conf.orig`, links `/etc/resolv.conf` to the resolved stub, and stops unless resolved has upstream servers and the package repository host still resolves. It links `/usr/bin/spx` to `/usr/local/bin/spx` where sudo's PATH leaves out `/usr/local/bin`, and takes the nbdkit plugin directory from `nbdkit --dump-config`, stopping when nbdkit cannot say. `SETUP_STAGES` runs a subset; `ISO_BUILD=1` runs it inside the ISO builder's chroot. |
| `setup-ovn.sh` | Bootstraps a compute node for OVN VPC networking: installs OVN/OVS, creates `br-int`, configures the WAN bridge (bridged, DHCP, or routed-NAT uplink), sets the chassis identity, and applies overlay sysctls. It runs OVS and OVN as root, setting `OVS_USER_ID` and `OVN_USER_ID` where `/etc/sysconfig` holds them, and hooks the socket-permission helper onto `ovs-vswitchd`, `ovn-controller`, `openvswitch-ipsec` and, on EL, `ovn-northd`. `--management` also starts the OVN central services, writes their `ovn-ctl` flags to `/etc/default/ovn-central` (Debian) or `/etc/sysconfig/ovn-northd` (EL), and records the role in `/etc/spinifex/ovn-role`. A later run without `--management` refuses while that file, or the `OVN_CTL_OPTS` line older nodes carry in `/etc/default/ovn-central`, records the role. It adds `maxsize 100M` to the OVS and OVN logrotate files, plus `rotate 5` where the file sets no count, and rotates as root, matching the daemons. On a host whose IPsec stack is libreswan it stops and masks `openvswitch-ipsec.service` and `ipsec.service`, because OVN IPsec does not run on libreswan yet; elsewhere it enables and starts `openvswitch-ipsec.service`, unless the daemon has masked it to keep IPsec off. The links it creates persist through systemd-networkd where that is enabled, and otherwise through `/etc/spinifex/hostnet.json`, which `spinifex-hostnet.service` (`spx admin hostnet apply`) replays at boot and on every OVS start; the first run records the choice in `/etc/spinifex/hostnet-backend`. Where NetworkManager runs, it leaves the links Spinifex owns unmanaged. |
| `install-node.sh` | Forms a multi-node cluster across hosts that are already running Spinifex: rebuilds the OVN database in clustered form, runs `spx admin init` on the first host and `spx admin join` on the rest, then verifies. Before touching any host it refuses hosts of different OS families (`distro_os_family`) or `spx` versions, and `--ipsec on` when any host uses libreswan. The scripted form of [install-multi-node](../docs/install/install-multi-node/README.md). Destroys the joining nodes' logical network state and crypto — see its `--help`. |
| `dev-install.sh` | Full local dev setup through the production installer: builds from source, packs a tarball, runs `setup.sh`, initialises the cluster, and starts services under systemd. |
| `clone-deps.sh` | Clones or updates the viperblock and predastore checkouts alongside spinifex for cross-repo development. |

### `lib/distro.sh`: host-OS capability probes

The node scripts select host-OS behaviour by probing for the capability, not by reading `/etc/os-release`; only `setup.sh`'s supported-OS gate keys on the OS ID.
Each probe prints its answer, or names every candidate it tried on stderr and returns 1, which it also does when it finds both candidates of a pair.

| Probe | Answers |
|---|---|
| `distro_pkg_manager` | `apt` or `dnf` |
| `distro_ovs_unit` | `openvswitch-switch.service` (Debian, Ubuntu) or `openvswitch.service` (EL) |
| `distro_ovn_central_layout` | `split` (Debian's `ovn-central` plus separate NB and SB units) or `lone-northd` (EL's single `ovn-northd.service`) |
| `distro_ca_trust` | the trust-store update command and its anchor directory |
| `distro_ovs_logrotate` | `/etc/logrotate.d/openvswitch-switch` or `/etc/logrotate.d/openvswitch` |
| `distro_ovn_logrotate` | `/etc/logrotate.d/ovn-common` or `/etc/logrotate.d/ovn` |
| `distro_os_family` | the OS family of the `/etc/os-release` text on stdin: `debian` for Debian and Ubuntu, otherwise the `ID` |

`distro_has_libreswan` is a predicate rather than a probe: it succeeds when libreswan's IKE daemon, `/usr/libexec/ipsec/pluto`, is installed.
EL's `openvswitch-ipsec` drives libreswan even beside strongSwan, so finding both is not an error.

`distro_missing_ovs_deps <unit-file>...` is a check rather than a probe: it prints each OVS or OVN unit the given unit files depend on or order against that is not installed, and fails only when a file cannot be read.
`setup-ovn.sh` runs it against the installed Spinifex units and stops when one is missing, because systemd silently drops ordering against a unit that does not exist; a missing `openvswitch-ipsec.service` is only a warning.

`setup-ovn.sh`, `uninstall-spx.sh`, `install-node.sh` and `dev-install.sh` source it from `lib/` beside themselves, then from `/usr/local/share/spinifex/lib/distro.sh`, where `setup.sh` installs it, and stop naming both paths when neither exists.
`setup.sh` is piped from the install endpoint before the tarball is downloaded, and `install-node.sh --wipe` copies `node-reset.sh` alone to each host, so those two carry inline copies of the probes they need between `# --- BEGIN distro.sh probes ---` markers.
`lib/distro_test.sh` fails when an inline copy differs from the library.
Go code uses `spinifex/runtime/host/hostos` for the same purpose.

### System packages

`setup.sh` installs `APT_RUNTIME_PACKAGES` with apt, or on Oracle Linux `OL_RUNTIME_PACKAGES` with dnf, choosing by `distro_pkg_manager`.
Before the dnf install it enables `ol10_UEKR8` and `ol10_kvm_utils`, excludes `qemu-kvm*` and `qemu-img*` from `ol10_appstream` so QEMU always comes from KVM Utilities, and writes `/etc/yum.repos.d/spinifex-nfv-sig.repo` for OVS 3.5 and OVN 25.03 from the CentOS NFV SIG.
That repo carries an `includepkgs=` allowlist, and its key is refused unless its fingerprint matches `NFV_SIG_KEY_FPR`.
It then installs `kernel-uek-modules` for the running kernel, stopping with a reboot instruction if that version has left the repositories, and loads `openvswitch`, `geneve`, `vhost_net` and `tun`.
`make install-system` takes the same dnf path, adding the build packages from CodeReady Builder.

| Script | What it does |
|---|---|
| `lint-apt-packages.sh <image>` | Resolves the apt list against a Debian or Ubuntu image's real indices with `apt-get install -s`. |
| `lint-dnf-packages.sh [image]` | Runs `setup.sh`'s Oracle Linux repository setup in an `oraclelinux:10` container, resolves the dnf list, and fails unless QEMU comes from KVM Utilities and OVS and OVN from the NFV SIG. |
| `check-glibc-ceiling.sh <ceiling> <elf>...` | Fails when an ELF file needs a glibc symbol version newer than the ceiling, naming the symbols. `build/Dockerfile.distro` runs it on `spx` and the nbdkit plugin with 2.34, so the one release tarball keeps loading on every supported OS. |

## Teardown

Both ship to `/usr/local/share/spinifex/` on every node, so they are available on a host installed with `curl … | bash` and not only from a checkout. They differ in what they take away:

| Script | What it does |
|---|---|
| `node-reset.sh` | Clears **state** and leaves the node installed: stops services, destroys guests, wipes `/etc/spinifex`, `/var/lib/spinifex`, `/var/log/spinifex` and the OVN databases, and removes the OVS bridges. It stops OVS and OVN central by the unit names the host's packages use. It leaves the NetworkManager unmanaged-device config, which the next `setup-ovn.sh` run rewrites, so NetworkManager cannot take a NIC back in between. The node is then ready for a fresh `spx admin init`. `--keep-data` preserves `/var/lib/spinifex`; `SPX_RESET_CONFIRM=reset` runs it unattended. |
| `uninstall-spx.sh` | Removes the **software** — the inverse of `setup.sh`. Delegates the state teardown above to `node-reset.sh` rather than reimplementing it, then removes the binary, the nbdkit plugin, the systemd units and slices, the service users, the sudoers grant, the sysctl/logrotate/chrony/udev drop-ins, the `systemd-networkd` units, the host firewall policy and its loaded nft table, the Spinifex systemd-resolved drop-in, and the Spinifex CA from the host trust store. It unmasks firewalld and leaves it disabled. Where `setup.sh` handed DNS to systemd-resolved, it restores the saved `/etc/resolv.conf` and turns resolved off. Verifies afterwards and exits non-zero naming anything that survived, then lists the host settings `setup.sh` changed that it leaves in place, such as the dnf repositories, `OVS_USER_ID`, masked IPsec units and the SELinux mode. |

`node-reset.sh` finds guests by process name, `qemu-system-*` or `qemu-kvm`, owned by the daemon's service user, and waits for them to exit before it deletes any state.
`node-reset_test.sh` checks that match against real processes, and that `reset-dev-env.sh` uses the same pattern.

`uninstall-spx.sh` keeps `/var/lib/spinifex` and every package by default — uninstalling software should not destroy data, and a `curl … | bash` install lands on hosts that were already using openvswitch, libvirt and chrony. `--purge-data` and `--purge-deps` opt into each, and both confirm separately even under `--yes` (`SPX_PURGE_CONFIRM=destroy` for unattended data removal). `--dry-run` prints the whole plan and touches nothing.
`--purge-deps` removes with apt and is refused on dnf hosts, where its fixed list cannot tell what `setup.sh` installed from what the host already had, and dnf would also remove every package that requires them.

**It refuses on a multi-node cluster, and `--yes` does not override that.** Uninstalling one node of a running cluster strands its guests and leaves the survivors reconciling against a chassis that will never answer. Run `spx admin cluster shutdown` first, then the uninstaller on each node.

## Dev environment

| Script | What it does |
|---|---|
| `reset-dev-env.sh` | Single-node reset: stops services, wipes `/etc/`, `/var/lib/`, `/var/log/` and `/run/spinifex`, rebuilds, reinstalls via `dev-install.sh`, and relaunches a smoke-test instance. Preserves the existing node/network topology from `spinifex.toml`. |
| `dev-env/*.conf` | Topology profiles for `reset-dev-env.sh --profile=NAME`, declaring which bridge carries the wan, lan and vpc planes: `single-nic` (all collapsed onto wan), `two-nic` (vpc folds onto lan), `three-plane` (a NIC each). |

## Verification and benchmarks

| Script | What it does |
|---|---|
| `smoke-test.sh` | End-to-end sanity check on a running node: imports an SSH keypair and the Ubuntu AMI, then launches an instance. `--create-vpc` builds the VPC, gateway, subnet and routes first and tears them down after; `--nodes N` launches through a spread placement group so each instance lands on a different physical server. `TEST_GPU=1` adds GPU passthrough checks. It runs the AWS CLI by absolute path, since sudo's `secure_path` leaves out `/usr/local/bin` on Oracle Linux. |
| `verify-network.sh` | Asserts a node's datapath matches its declared planes — EIP reachability, which plane carries Geneve, and which interfaces services bind. Catches what `smoke-test.sh` cannot, since none of its traffic leaves the box. |
| `check-coverage.sh` | Fails the build if total Go coverage falls below the minimum threshold, excluding packages exempt from the requirement. |
| `diff-coverage.sh` | Coverage check scoped to changed lines only, against an auto-detected base ref (`HEAD~1` on main, `origin/main` on dev, otherwise `origin/dev`). |
| `sync-aws-models.sh` | Replaces the embedded AWS Smithy models under `internal/awsmodel/models/` with the ten service files from `aws/api-models-aws` at the given commit, and records that commit in `internal/awsmodel/model_source.go`. Takes a full 40-character SHA. |
| `run-bench.sh` | Launches a benchmark against running Ubuntu instances, auto-detecting the AMI for the host architecture. Tracks nbdkit and perf alongside the run. |
| `disk-performance.sh` | Runs a `fio` random 70/30 read-write benchmark swept across 4k/16k/128k/1M. Writes JSON results to `/tmp/spinifex-disk-bench`; override with `BENCH_DIR`, `OUT_DIR`, `SIZE`, `JOBS`, `BLOCK_SIZES`. |
| `network-performance.sh` | iperf throughput from several clients to one server, driven over SSH, writing a `summary.txt` of Gbit/s per client. `--server-ip` sets the address clients dial, so pointing it at instance private addresses measures the VPC overlay rather than the external pool. |

## Guest images

| Script | What it does |
|---|---|
| `build-system-image.sh` | Builds a minimal system AMI (Alpine or Ubuntu) from a manifest. All customisation happens inside the libguestfs appliance, so the build touches no host block device, no host mount, and needs no sudo. `--import` also registers it as an AMI. |
| `publish-system-image.sh` | Converts a built raw image to compressed qcow2, writes the `.sha256` sidecar the catalog verifier expects, and uploads both to the R2 bucket behind `iso.mulgadc.com/system-ami/`. |
| `build-microvm-image.sh` | Builds the minimal Alpine kernel + initramfs used by the QEMU microvm machine type and the lb-agent, carrying haproxy (ALB) and nginx-stream (NLB) data planes. |
| `build-qboot.sh` | Builds qboot, the firmware the daemon passes to every microvm with `-bios`, from a pinned upstream commit in a digest-pinned Debian container. Fails unless the output matches the recorded sha256, which is also Debian 13's `qboot.rom`. Writes `qboot.rom` and its GPL-2.0 licence beside the microvm kernel, and the matching source tarball, which releases publish, under `build/qboot/`. `make build-microvm-image` runs it. |
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
| `rds-mariadb/` | The `spinifex-rds-mariadb` AMI. Same layout as `rds-postgres/`, plus the `/etc/my.cnf.d` include drop-in and the `conf.d/mariadb` dependency that keeps the packaged service off an unbootstrapped datadir. |
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

## `demo/` — GPU demo

| Script | What it does |
|---|---|
| `deploy.sh` | Deploys and starts the four-instance GPU demo (dashboard, YOLO, Ollama, qwen3-vl) from the `*_IP` and `VIDEO_PATH` env vars. |
| `dashboard_server.py` | Serves the demo web UI and proxies the streams from the GPU instances. |
| `yolo_stream.py` | Runs YOLO detection over a video file, serving an MJPEG stream of annotated frames and an SSE stream of qwen3-vl scene descriptions. |
