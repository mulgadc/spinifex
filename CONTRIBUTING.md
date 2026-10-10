# How to Contribute

We welcome contributions to Spinifex!

Report security vulnerabilities privately through [SECURITY.md](SECURITY.md), not as a public issue.

**AWS parity is the design rule.** When Spinifex's behaviour is in question, the answer is what AWS does. If you believe Spinifex diverges from AWS, state what AWS does and show it with a test or a reproduction against a running cluster.

## Understanding the Codebase

Every AWS call enters through a SigV4-authenticated gateway, is published to NATS, and is answered by the daemon that claims it. Start with these:

| Doc | Covers |
|---|---|
| [`docs/DESIGN.md`](docs/DESIGN.md) | Architecture, request flow, daemons, storage integration, file layout. |
| [`docs/COMMANDS.md`](docs/COMMANDS.md) | The `spx admin` CLI and the systemd units it manages. |
| [`docs/coverage/`](docs/coverage/README.md) | Which AWS API operations each service implements. |
| [`scripts/README.md`](scripts/README.md) | Install, dev-environment, and image-build scripts. |
| [`docs/PACKAGE_BOUNDARY_MIGRATION.md`](docs/PACKAGE_BOUNDARY_MIGRATION.md) | ADR-0001 implementation inventory and incremental source moves. |

User-facing docs are published at [docs.mulgadc.com](https://docs.mulgadc.com).

## Development Setup

These steps build Spinifex from source and run it as a single-node cluster, following the [Source Install](https://docs.mulgadc.com/docs/install-source) guide.

### Prerequisites

- **Ubuntu 26.04** or **Debian 13**, on x86_64 or aarch64
- A host with hardware virtualisation (KVM) and `sudo` access
- The host's WAN interface enslaved to a Linux bridge named `br-wan`, with the host IP, default route and DHCP on the bridge rather than the bare NIC (see [VPC Networking → Bridge Setup](https://docs.mulgadc.com/docs/vpc-networking#bridge-setup-physical-network-wiring))

Check the bridge before continuing:

```bash
ip -br link show br-wan   # bridge exists and is UP
ip route                  # default route's dev is br-wan
```

### Install and Run

```bash
mkdir -p ~/Development/mulga && cd ~/Development/mulga
git clone https://github.com/mulgadc/spinifex.git
sudo make -C spinifex quickinstall        # system packages, Go 1.27, AWS CLI v2
export PATH=$PATH:/usr/local/go/bin

cd spinifex
./scripts/clone-deps.sh                   # sibling repos: viperblock, predastore, northstar, bluebottle
./scripts/dev-install.sh                  # build, install, init a one-node cluster, start services

AWS_PROFILE=spinifex aws ec2 describe-instance-types
```

A list of instance types means the cluster is up.

`make preflight` also needs `shellcheck` and `golangci-lint` (CI pins v2.13.0):

```bash
sudo apt-get install -y shellcheck
curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b "$(go env GOPATH)/bin" v2.13.0
```

The web console additionally needs Node.js 24 and `pnpm`.

## Development Workflow

| Command | Use it to |
|---|---|
| `make deploy` | Rebuild, install `spx`, and restart `spinifex.target` after a code change. |
| `make reinstall` | Re-run `dev-install.sh` after changing systemd units, helper scripts, or logrotate config. |
| `./scripts/reset-dev-env.sh` | Wipe all cluster state and reinstall from scratch, keeping your network topology. |
| `make test` | Run unit tests (`make test-race` for the race detector). |
| `make test-integration` | Run the real gateway router against embedded NATS JetStream. |
| `make preflight` | Run the pre-commit gate: lint, `govulncheck`, coverage, and manifest and script checks. Must pass. |

Follow the services with `journalctl -u 'spinifex-*' -f`.

The web console lives in `spinifex/runtime/roles/spinifexui/frontend/` (`pnpm install`, `pnpm dev`, `pnpm lint`, `pnpm test`). `spx` embeds its checked-in `dist/`, so run `make build-ui` and commit the rebuilt `dist/` with your change.

## Creating a Pull Request

1. Fork [mulgadc/spinifex](https://github.com/mulgadc/spinifex) and clone your fork into the same parent directory as the sibling repositories, branching off `dev`:

   ```bash
   cd ~/Development/mulga
   git clone git@github.com:YOUR_USERNAME/spinifex.git && cd spinifex
   git remote add upstream git@github.com:mulgadc/spinifex.git
   git fetch upstream && git checkout -b feat/my-feature upstream/dev
   ```

2. Make your change, then verify it:
   - `make preflight` passes.
   - `go mod tidy` leaves only intended changes in `go.mod` and `go.sum`, with none of the `clone-deps.sh` `replace` lines.
   - Runtime changes are exercised on your dev cluster with `make deploy`.

3. Commit using [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `refactor:`, `docs:`, `chore:`), push, and open a pull request against `dev`, filling in the template.
