# Spinifex on Vultr

`deploy.sh` builds a single-node Spinifex deployment on a Vultr VX VM in its own VPC, and tears it down again leaving nothing billing.

```bash
export VULTR_API_KEY=...            # the key's IP allowlist must admit this host and the node
./deploy.sh up      --name v1 --distro spinifex-<version>.tar.gz
./deploy.sh status  --name v1
./deploy.sh destroy --name v1
```

`up` prints the endpoint and writes an AWS profile `spinifex-vultr-<name>` with the cluster's CA at `~/.spinifex/vultr-<name>-ca.pem`, so `aws --profile spinifex-vultr-<name> ec2 describe-instances` works straight away. `--channel latest|dev` installs a published release instead of a local tarball.

## What it creates

| Object | Created by | Removed by |
| --- | --- | --- |
| VPC `spinifex-<name>` (`10.10.8.0/24` unless `--vpc-subnet`) | Terraform | Terraform |
| SSH key `spinifex-<name>` | Terraform | Terraform |
| VM `spinifex-<name>-node1`, plan `vx1-g-16c-64g` | Terraform | Terraform |
| Boot volume `spinifex-<name>-node1-boot` (50 GB) | Terraform, through the VM | `destroy`: Vultr keeps it after the VM is deleted |
| Reserved IPs labelled `spinifex-vultr-<name>:` | Spinifex, for each public IP and Elastic IP | Spinifex on release; `destroy` sweeps any left |

`destroy` fails, and lists what remains, if anything above is still in the account afterwards.

## Where things live

- **State** is per deployment under `~/.spinifex/vultr/<name>/`: the Terraform state, the node's SSH host key, logs, and one `step.*` file per completed step. A failed `up` resumes from the step that failed.
- **The API key** is copied to `/etc/spinifex/vultr/api-key` on the node over SSH. It is never a Terraform variable, so it is in neither the state nor the instance metadata.
- **The host firewall** is armed before Spinifex first starts. The VM has no Vultr firewall group, so it is the only one.

## Requirements

- OpenTofu 1.5 or newer, the AWS CLI v2, `python3` and an SSH key (`--ssh-key`, default `~/.ssh/id_ed25519`).
- A plan with at least 16 vCPU. Smaller VX plans report their vCPUs as hyperthreads and Spinifex schedules half of them.
