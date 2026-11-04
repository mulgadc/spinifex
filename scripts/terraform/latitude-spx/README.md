# latitude-spx

Builds a Spinifex deployment on latitude.sh bare metal from your laptop with `deploy-latitude.sh`.
The commands, flags, step markers and state layout follow `vultr-spx`.

**Status: not yet run end to end.**
The script was written from the plan doc findings (`docs/development/feature/latitude-bm.md`, L1 to L26) and from the provider schema.
No latitude.sh API call or host run was made while building it, so treat the first `up` as the test.
One node has been the only shape run live, and only in part (see Unverified). The three-node flow, including the floating pool, has not been run: Ben takes that over.

## What it creates

| Resource | Notes |
|---|---|
| Project | `spinifex-<name>`, unless `--project` names an existing one (then it is never deleted) |
| SSH key | `spinifex-<name>`, account scoped |
| User data | `spinifex-<name>`, host preparation only (no `{{ }}`, latitude.sh substitutes them) |
| Servers | `m4-metal-medium`, site `DAL`, Ubuntu 26.04, hourly billing, hostnames `spx-<name>-nodeN` |
| Elastic IP | one by default (`elastic_ip_count` in `variables.tf`, 1 to 5; set `TF_VAR_elastic_ip_count` to change it). With one node it is a static pool address and the count must stay 1, because a second pool is never tried when the first is full; with three nodes the addresses form the `latitude` floating pool, which Spinifex moves between servers. `nginx-webserver` needs one address and runs on one node; `nginx-alb` needs two public addresses and is a three-node workbook |
| Virtual network | three nodes only: `spinifex-<name>`, one VLAN assignment per server |

## Requirements

OpenTofu (or Terraform) 1.5 or later, `aws` CLI, `python3`, `curl`, `ssh`, an SSH key pair.

| Variable | When | Meaning |
|---|---|---|
| `LATITUDESH_AUTH_TOKEN` | always | the account API token; used by the provider and this script only, never sent to a node |
| `LATITUDESH_NODE_TOKEN` | `--nodes 3` | a second, different, writable API key, copied to each node (`/etc/spinifex/latitude/api-token`, owned by `spinifex-daemon` with mode 0400, in a root-owned 0711 directory; the other services share the `spinifex` group, so no group may read it; ownership and mode are set on every `up`; a symlink or any other owner there is refused) so the daemon can move Elastic IPs |

The script refuses a node key it can see is read-only (it lists `/auth/api_keys` and matches the key's last five characters). If the key is not among the account's keys, write authority is first proven by the first Elastic IP move.

Exposure: a latitude.sh API key is team-wide. It is not scoped to Elastic IPs, so anyone who reads it from a node can do whatever the key's owner can in the team (the API exposes `read_only` and `allowed_ips` on a key, and no narrower scope). Create the node key with `allowed_ips` set to the nodes' public addresses, and revoke it after `destroy`.

Nodes are reached as `ubuntu` with the deployment's SSH key, and remote commands run through passwordless `sudo`: latitude.sh puts the key on the image's login user and root login is not enabled.

## Usage

```
export LATITUDESH_AUTH_TOKEN=...        # from the latitude.sh dashboard
./deploy-latitude.sh up --name demo     # one node

export LATITUDESH_NODE_TOKEN=...        # a second, writable key
./deploy-latitude.sh up --name demo3 --nodes 3
./deploy-latitude.sh status --name demo
./deploy-latitude.sh ssh --name demo node1 'systemctl status spinifex.target'
./deploy-latitude.sh credentials --name demo
./deploy-latitude.sh stop node1 --name demo
./deploy-latitude.sh start node1 --name demo
./deploy-latitude.sh destroy --name demo
```

Options: `--name`, `--site` (alias `--region`), `--plan`, `--os`, `--project ID|SLUG`, `--distro TARBALL`, `--channel`, `--spx-region`, `--spx-az`, `--ssh-key`, `--image`, `--data-disk-id`, `--vpc-subnet` (the VLAN's /24, three nodes), `--nodes 1|3`, `--yes`.
`./deploy-latitude.sh --help` lists them with defaults.

Three nodes need `--distro` with a tarball built from this branch (`make distro-amd64`) until a release carries the latitude provider.
A released spx rejects `source="latitude"` and its daemon restarts in a loop, so `up --nodes 3` without `--distro` stops in preflight.
Before the pool is written, each node's `/usr/local/bin/spx` is checked for the `latitude_elastic_ip_ids` key name; spx has no validate subcommand, so this is a string search of the binary, not a run of its config check.

## What `up` does

`terraform` then, per node: `host` (wait for SSH as `ubuntu` and cloud-init, `/dev/kvm`), `disk`, `vlan` (three nodes), `install`, `form`, `start`; then `pool` (one node) or `cluster` (three nodes: `install-node.sh` forms the cluster); three nodes then install the node token on every `up` and add the `latitude` pool (`pool`); then `restart` (three nodes wait for the current daemon to log "floating pool ready" for the pool), `public`, `image` and the AWS profile.
Each step writes `step.<name>[.nodeN]` in the state directory, and a rerun resumes at the first step without one.

- **Data disk.** `host-data-disk.sh` picks the blank NVMe by content and by-id EUI, never by `nvmeXn1` (L1, L11).
A candidate is at least 100 GB with no signature, partitions, holders or mounts, and not in the root filesystem's ancestry; with none it stops and lists the disks.
With several (LON2 has two 1.7 TB and one spare 447 GB blank NVMe next to the root disk) it takes the largest, the lowest by-id path among equals, prints what it chose and the blank disks it left, and never touches the others (`--data-disk-id` chooses another).
It mounts by UUID with `nofail` and the script records the UUID in `data_uuid.<node>`.
- **Mount check.** Before anything writes under `/var/lib/spinifex`, and on every run, the mounted filesystem UUID must equal the recorded one.
The units that write there also get a `RequiresMountsFor=/var/lib/spinifex` drop-in.
The mount is by UUID in fstab, so renamed disks do not change it, and there is no check at service start.
- **Firewall.** `setup.sh --firewall on` runs before Spinifex first starts.
- **One node.** The Elastic IP is a static pool (`gateway_ip = "192.0.2.1"`, the node's next hop as `gateway`, no `prefix_len`) with the IMDS addresses moved off the ones latitude.sh routes.
- **Three nodes.** The `vlan` step makes `eno2.<vid>` with `ip link` and persists it as systemd-networkd files for that link plus a drop-in on the `eno2` unit.
It never runs `netplan apply`, refuses a subnet that overlaps the SSH peer or a route, and removes the VLAN again if the public interface's address, default route or the route to the SSH peer differ afterwards.
`form_cluster` then puts the cluster and Geneve planes on that VLAN with `install-node.sh` (`--user ubuntu`); its output is not kept, as it can hold the join token.
- **Floating pool (three nodes).** The `latitude` pool names the Elastic IP IDs and the project ID from the Terraform outputs, and each node's `provider_host_id` is its server ID. `spinifex.toml` is edited idempotently and read back. Failover moves an address with one `PATCH /elastic_ips/{id}` and waits for it to be active; latitude.sh has no unassign, so a node that loses an address is not told to release it.
- **State** is in `~/.spinifex/latitude/<name>/`: Terraform state, `ids` (every `latitudesh_*` object in the state, merged on each apply and before each destroy), `step.*` markers, logs and the data disk UUIDs.
- **AWS profile** `spinifex-latitude-<name>` is replaced on each `up`, with the CA at `~/.spinifex/latitude-<name>-ca.pem`.
The secret key is written to `~/.aws/credentials` (0600, other profiles kept, the old file saved beside it) and never appears in a process argument.

## Token handling

`LATITUDESH_AUTH_TOKEN` is read from the environment by the provider and by the script's API calls.
It is not a Terraform variable, is on no command line (curl reads it from stdin), is not sent to a node, and is written to no file.
`LATITUDESH_NODE_TOKEN` is checked with curl reading it from stdin and reaches each node on stdin of the install script, so it is in no argument on either side.
`TF_LOG` is unset by the script.
The join token that `install-node.sh` creates is shared with Vultr and OCI: its init log is root-only, and `spx admin join` still takes it as `--token` on the joining node (tracked separately).

## Recovery

If SSH is lost once the firewall is armed, use the serial console.
`up` enables the server's out-of-band connection with the deployment's SSH key and prints the `ssh` command it returns before it arms the firewall, falling back to the Out-of-Band tab in the dashboard.
Logging in there needs an OS user password, which you set yourself while SSH still works.
A fresh deployment that cannot be recovered is cheaper to `destroy` and `up` again.
The VLAN step prints the console command first.
It refuses a subnet that holds the SSH peer or overlaps an existing route, and removes the VLAN again if the route to the SSH peer or the public interface changes.

## destroy

`destroy` asks you to type the deployment name (`--yes` skips only that), then records every ID in the Terraform state and stops without destroying anything if it cannot.
It runs `terraform destroy`, fetches every recorded ID by itself (2xx present, 404 gone, anything else fails and keeps the records), and deletes what remains of those.
Servers, keys, user data, virtual networks and projects whose names match the deployment but that are not recorded are listed by ID and deleted only after you type `delete-untracked` for exactly those IDs, which `--yes` never supplies; an ID that shows up later needs its own confirmation, and without a terminal they are kept and `destroy` exits 1.
Elastic IPs are touched only by recorded ID.
Elastic IP release is asynchronous and only works once the address is active or in error, so the sweep retries.
Because the Elastic IP list ignores pagination, `destroy` ends by asking you to check the dashboard for leftovers and type `checked`.
An `up` that failed while creating can leave an object nothing recorded, such as an Elastic IP; that dashboard check is the only thing that finds it.

## Unverified

Nothing in the three-node flow has run against real servers. For one node, only the parts the person ran have been exercised, and the script has changed since.

- The OS slug `ubuntu_26_04_x64_lts` (the `latitudesh_operating_system` data source fails the plan if it is wrong).
- That the deploy key is on `ubuntu` and `sudo -n` works for it, and that `sudo` resets nothing the script needs (the SSH peer address is read before `sudo` for that reason).
- The power action request (`POST /servers/{id}/actions` with `power_off` and `power_on`).
- Resolving `--project` as a slug through `GET /projects/{slug}`, and the `in_stock` shape of `GET /plans` used by the stock check, which only warns when it cannot read it.
- The `GET` and `DELETE` paths for `/user_data/{id}` and `/virtual_networks/{id}`.
- That the Spinifex API certificate names the node's public address, which the profile check needs.
- That the private NIC is `eno2`, its tagging and MTU (the script uses 1500), and that the networkd files persist the VLAN across a reboot.
- `GET /auth/api_keys` and its `token_last_slice` and `read_only` fields, which the node key check relies on; and that a key with `allowed_ips` set to the nodes' addresses still works from them.
- The Elastic IP move itself: `PATCH /elastic_ips/{id}` with `server_id`, its 202 then `active`, the error codes, and how long a move takes; the client waits up to its move timeout and makes one write per call.
- That the daemon reads the node token and logs "floating pool ready", then fails over when a node stops (`stop node1`) and the address lands on another node.
- `install-node.sh` as `--user ubuntu` on latitude.sh servers.

## Not exercised

Any real API call from the three-node flow, `destroy`, `host-data-disk.sh` on a host and the VLAN step on a server.
Guest launch, public reachability of the Elastic IP, the Terraform workbooks and failover are not part of `up`; prove them by hand.
`e2e-latitude.sh` and removing the old `install-node.sh` in this directory are not done.

## Tests

`sh node-scripts_test.sh` checks the node token install (install, unchanged, rotation, modes, an older install converging, symlinks and owners refused; run it as root too, with `sudo sh node-scripts_test.sh`, to check that only the daemon's user can read the file) and the readiness check (old success then current failure) with stand-in tools.
`bash deploy-argv_test.sh` checks that neither API token, nor the five characters used to find the node key, is an argument of any external command in the node token paths, and that the SSH peer address comes from the same connection after sudo's environment reset.
`scripts/install-node_test.sh` (part of `make preflight`) checks that the join token reaches `spx admin join` only on stdin, so sudo's log never holds it.
The first two are not part of `make preflight`.
The Go side: `cd spinifex && GOWORK=off go test ./providers/cloud/latitude/ ./domains/network/external/floating/ ./bootstrap/config/ ./runtime/daemon/`.
