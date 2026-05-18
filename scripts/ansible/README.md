# Ansible Dev Lifecycle (parallel to bash scripts)

Experimental Ansible-based lifecycle for a single-node Spinifex dev host.
Lives alongside the existing `scripts/dev-*.sh` and `scripts/reset-dev-env.sh`;
neither set is removed. Decide which to keep after parity testing.

Plan: `docs/development/improvements/ansible-dev-lifecycle.md` (mulga top-level).
Bead: `mulga-siv-9`.

## Prerequisites

Venv lives at sub-repo root (`spinifex/.venv/`) — one venv per sub-repo,
shared by any future python tooling in spinifex. Bootstrap from `spinifex/`:

```
sudo apt install python3.13-venv
python3 -m venv .venv
source .venv/bin/activate
pip install 'ansible>=9'
ansible-galaxy collection install -r scripts/ansible/requirements.yml
```

Then `cd scripts/ansible` to run playbooks — `ansible.cfg` resolves the
inventory and roles relative to CWD.

## Playbooks

| Playbook | Purpose | Bash parity |
|---|---|---|
| `playbooks/dev-preflight.yml` | Dependency + port check | `dev-setup.sh` §checks |
| `playbooks/dev-teardown.yml` | Wipe state only (no reinstall) | `reset-dev-env.sh` §teardown |
| `playbooks/dev-install.yml` | Build + install + init + smoketest on clean box | `dev-install.sh` |
| `playbooks/dev-reset.yml` | Capture settings → teardown → build → install → init → smoketest | `reset-dev-env.sh` (full) |
| `playbooks/dev-deploy.yml` | Rebuild + swap binaries/microvm artifacts + restart (no setup.sh, no smoketest) | `make deploy` |
| `playbooks/dev-status.yml` | Read-only health snapshot (units, ports, OVN/OVS, gateway, counts, drift) | none |
| `playbooks/dev-logs.yml` | Dump journald + `/var/log/spinifex/*` + status/networking/versions into a tarball | none |
| `playbooks/dev-snapshot.yml` | Quiesce spinifex+OVN+OVS, tar state paths to a named bundle, restart | none |
| `playbooks/dev-restore.yml` | Stop services, wipe + untar a named bundle, refresh CA, restart | none |
| `playbooks/dev-version.yml` | Build-vs-install drift report (spx, plugin, microvm) + sub-repo HEADs | none |

Upcoming (not yet implemented):

- `dev-mode-start.yml` / `dev-mode-stop.yml` — parity with `start-dev.sh` / `stop-dev.sh`
  (lowest priority — deprecated bash scripts, CI-only callers).

## Invocation

```
ansible-playbook playbooks/dev-preflight.yml
ansible-playbook playbooks/dev-teardown.yml
ansible-playbook playbooks/dev-install.yml
ansible-playbook playbooks/dev-reset.yml
ansible-playbook playbooks/dev-deploy.yml
ansible-playbook playbooks/dev-status.yml
ansible-playbook playbooks/dev-logs.yml
ansible-playbook playbooks/dev-snapshot.yml -e snapshot_name=before-merge
ansible-playbook playbooks/dev-restore.yml  -e snapshot_name=before-merge
ansible-playbook playbooks/dev-version.yml
```

Or via `make` (from `spinifex/`):

```
make ansible-dev-preflight
make ansible-dev-teardown
make ansible-dev-install
make ansible-dev-reset
make ansible-dev-deploy
make ansible-dev-status
make ansible-dev-logs
make ansible-dev-snapshot ANSIBLE_EXTRA='-e snapshot_name=before-merge'
make ansible-dev-restore  ANSIBLE_EXTRA='-e snapshot_name=before-merge'
make ansible-dev-version
```

### When to use which

- First-time / clean box → `ansible-dev-install`
- Iterate on Go code, microvm initramfs, lb-agent → `ansible-dev-deploy` (fast)
- Changed systemd units, helper scripts, logrotate, setup.sh → `ansible-dev-reset` (slow, full rebuild)
- Need a clean slate without reinstall → `ansible-dev-teardown`
- "Is my dev box healthy?" → `ansible-dev-status` (read-only, never mutates)
- Filing a bug / capturing the state of a broken box → `ansible-dev-logs` (writes `/tmp/spinifex-logs-<host>-<ts>.tar.gz`; override `-e logs_since=10min` or `-e logs_include_dmesg=false`)
- Checkpoint before a risky op (branch switch, viperblock surgery, ovn restart cycle) → `ansible-dev-snapshot ANSIBLE_EXTRA='-e snapshot_name=<name>'`; roll back via `ansible-dev-restore` instead of a full `dev-reset`. Bundles live in `/var/lib/spinifex-snapshots/<name>.tar.gz` (root-owned, survives `dev-teardown`).
- "Do I need to `ansible-dev-deploy`?" → `ansible-dev-version` (drift-only check; faster than `dev-status`)

## Variable overrides

Defaults in `vars/defaults.yml`. Override per run with `-e`:

```
ansible-playbook playbooks/dev-teardown.yml -e spinifex_wipe_ssh_keys=true
```

Useful overrides:

| Var | Default | What it does |
|---|---|---|
| `spinifex_wipe_legacy_home` | `true` | Remove `$HOME/spinifex` (legacy dev-mode path) |
| `spinifex_wipe_ssh_keys` | `false` | Remove `~/.ssh/spinifex-key*` |
| `spinifex_wipe_aws_creds` | `false` | Remove spinifex profile from `~/.aws/credentials` |
| `teardown_process_wait_seconds` | `30` | Grace period for processes to exit |
| `spinifex_region` | `ap-southeast-2` | Region passed to `spx admin init` |
| `spinifex_az` | `{{ region }}a` | AZ passed to `spx admin init` |
| `spinifex_external_mode` | `pool` | `pool` \| `nat` external networking |

`dev-reset.yml` automatically captures `region`, `az`, `external_mode`,
pool range, gateway, prefix-len, nat gateway_ip from the existing
`/etc/spinifex/spinifex.toml` before teardown and replays them into
`init`. Use `-e` to override.

## Helper scripts

`scripts/aws-as-spinifex.sh` and `scripts/spx-as-spinifex.sh` wrap `aws`
and `spx` invocations in `sg spinifex -c '...'` so they run with the
`spinifex` supplementary group active. Both helpers preset
`AWS_PROFILE=spinifex`. All AWS-calling tasks in the roles use these
via `spinifex_aws_cli` / `spinifex_spx_cli` from `vars/defaults.yml`.

Background: `/etc/spinifex/ca.pem` lives under `drwxr-x--- root:spinifex`,
so the AWS CLI needs `spinifex` group membership to read the CA bundle.
Ansible's `become_user` is a no-op when the target user equals the
calling user (no `sudo -u` invoked → no `initgroups(3)` → supplementary
groups inherited from the operator's shell). On a fresh login the
operator's shell would have `spinifex` (added by `setup.sh`); on shells
that predate the group add it does not. The helpers sidestep this
entirely. See bead `mulga-siv-92`.

## Leak catalog

`roles/teardown/README.md` tracks every known state source. When teardown
misses something, add it there and in `roles/teardown/tasks/main.yml`.
