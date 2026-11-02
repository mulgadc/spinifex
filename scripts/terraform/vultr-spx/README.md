# Spinifex on Vultr

`deploy-vultr.sh` builds a one- or three-node Spinifex deployment on Vultr VX VMs or bare-metal servers in their own VPC, and tears it down again leaving nothing billing. The customer guide is [`docs/vultr-integration/README.md`](../../../docs/vultr-integration/README.md).

```bash
export VULTR_API_KEY=...            # the key's IP allowlist must admit this host and every node
./deploy-vultr.sh up                                                 # one VX VM, latest release
./deploy-vultr.sh up --nodes 3 --plan vbm-8c-64gb-amd                # three bare-metal servers
./deploy-vultr.sh up --channel dev                                   # the newest dev build
./deploy-vultr.sh up --name v1 --distro spinifex-<version>.tar.gz    # a named deployment from a local tarball
./deploy-vultr.sh status
./deploy-vultr.sh credentials
./deploy-vultr.sh ssh [nodeN] [command]
./deploy-vultr.sh destroy
```

Without `--name` the deployment is called `default`, and `up` writes the AWS profile `spinifex`. A named deployment writes `spinifex-vultr-<name>` instead, so several can run side by side. Either way the cluster's CA is at `~/.spinifex/vultr-<name>-ca.pem` and the profile works straight away. `up` ends by printing every node's address and the `/32`s the API key's allowlist needs.

## What it creates

| Object | Created by | Removed by |
| --- | --- | --- |
| VPC `spinifex-<name>` (`10.10.8.0/24` unless `--vpc-subnet`) | Terraform | Terraform |
| SSH key `spinifex-<name>` | Terraform | Terraform |
| VMs or bare-metal servers `spinifex-<name>-node1` to `node3` | Terraform | Terraform |
| VM boot volumes `spinifex-<name>-nodeN-boot` (50 GB each) | Terraform, through the VM | `destroy`: Vultr keeps it after the VM is deleted |
| Reserved IPs labelled `spinifex-vultr-<name>:` | Spinifex, for each public IP and Elastic IP | Spinifex on release; `destroy` sweeps any left |

A `vbm-` plan selects bare metal. It boots from local NVMe, so it has no boot volume, and Spinifex keeps its data on the root disk. `destroy` fails, and lists what remains, if anything above is still in the account afterwards.

## Where things live

- **State** is per deployment under `~/.spinifex/vultr/<name>/`: the Terraform state, the nodes' SSH host keys, logs, and one `step.*` file per completed step. A failed `up` resumes from the step that failed.
- **The API key** is copied to `/etc/spinifex/vultr/api-key` on each node over SSH. It is never a Terraform variable, so it is in neither the state nor the instance metadata.
- **The host firewall** is armed before Spinifex first starts. The nodes have no Vultr firewall group, so it is the only one.

## Requirements

- OpenTofu 1.5 or newer, the AWS CLI v2, `python3` and an SSH key (`--ssh-key`, default `~/.ssh/id_ed25519`).
- A VX plan with at least 16 vCPU, of which Spinifex offers 14 to instances; an 8-vCPU plan offers 7.
