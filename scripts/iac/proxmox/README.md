# Proxmox IaC

OpenTofu/Terraform templates for provisioning Spinifex development clusters on Proxmox VE.

## Prerequisites

- [OpenTofu](https://opentofu.org/) (or Terraform >= 1.6.0)
- 1 or more Proxmox VE nodes with:
  - A Terraform API token (`Datacenter > Permissions > API Tokens`)
  - SSH access for a `terraform` user on each node
  - Debian 13 cloud image uploaded to `local:iso/` on each node
- SSH keypair for cloud-init VM access
- A hardcoded Debian ISO is used, which needs to be pre-installed on each Proxmox host ("debian-13-genericcloud-amd64-20260518-2482.img") https://cloud.debian.org/images/cloud/trixie/20260518-2482/debian-13-genericcloud-amd64-20260518-2482.qcow2

## Quick Start (spinifex-test.sh)

The `spinifex-test.sh` wrapper script manages the full lifecycle: provision, configure, test, and destroy.

```sh
# Source environment
source scripts/iac/proxmox/.env
source ~/prox   # Proxmox API token

# Provision a 3-node cluster
./scripts/iac/spinifex-test.sh up my-cluster

# Configure: clone repo, build, form cluster, start services
./scripts/iac/spinifex-test.sh configure my-cluster

# Run smoke tests
./scripts/iac/spinifex-test.sh test my-cluster

# Check cluster health
./scripts/iac/spinifex-test.sh status my-cluster

# SSH to a specific node
./scripts/iac/spinifex-test.sh ssh my-cluster 2

# Destroy when done
./scripts/iac/spinifex-test.sh down my-cluster

# Or run the full lifecycle (up → configure → test → down)
./scripts/iac/spinifex-test.sh full my-cluster
```

### Options

```
--node-count=N       Number of VMs (default: 3)
--memory-mb=N        Memory per VM in MB (default: 16384)
--cpu-cores=N        CPU cores per VM (default: 4)
--disk-size-gb=N     Disk size per VM in GB (default: 32)
```

Example with custom sizing:

```sh
./scripts/iac/spinifex-test.sh up my-cluster --node-count=5 --memory-mb=8192 --cpu-cores=2
```

Per-cluster state is stored in `scripts/iac/proxmox/clusters/<name>/terraform.tfstate`, allowing multiple clusters to coexist.

## Manual Setup

### 1. Configure environment

```sh
cp .env.example .env
# Edit .env with your Proxmox endpoint, SSH keys, and node configuration
```

### 2. Set API token

```sh
export PROXMOX_VE_API_TOKEN="terraform@pve!provider=YOUR_TOKEN_SECRET"
```

### 3. Deploy

```sh
source .env
tofu init
tofu plan -var="cluster_name=dev1"
tofu apply -var="cluster_name=dev1"
```

### Example `.env` file

```sh
# --- Proxmox API ---

# API token - set manually via: export PROXMOX_VE_API_TOKEN="terraform@pve!provider=..."
# Format: <user>@<realm>!<token-name>=<token-secret>

# Proxmox VE API endpoint (the URL you use to access the web UI)
export TF_VAR_proxmox_endpoint="https://pve1.lab.example.com:8006/"

# --- SSH Access ---

# SSH username on the Proxmox hosts (used by the provider for file uploads via SCP)
export TF_VAR_proxmox_ssh_username="terraform"

# Path to SSH private key that authenticates as the above user on each Proxmox host
export TF_VAR_ssh_private_key_path="~/.ssh/proxmox-tf"

# Path to SSH public key injected into VMs via cloud-init (the tf-user account)
export TF_VAR_ssh_public_key_path="~/.ssh/proxmox-tf-cloudinit.pub"

# --- Proxmox Nodes ---
#
# Physical Proxmox hosts where VMs are placed (round-robin).
# Each node object:
#   name         - Proxmox node name exactly as shown in the Proxmox UI sidebar
#   address      - SSH-reachable hostname or IP of the Proxmox host
#   bridge       - Linux bridge for VM network interfaces (check Proxmox UI > Node > Network)
#   datastore_id - Proxmox storage pool for VM disks (check Proxmox UI > Node > Storage)

export TF_VAR_nodes='[
  {
    "name": "pve1",
    "address": "pve1.lab.example.com",
    "bridge": "vmbr0",
    "datastore_id": "local-lvm"
  },
  {
    "name": "pve2",
    "address": "pve2.lab.example.com",
    "bridge": "vmbr0",
    "datastore_id": "local-lvm"
  },
  {
    "name": "pve3",
    "address": "pve3.lab.example.com",
    "bridge": "vmbr0",
    "datastore_id": "local-lvm"
  }
]'
```

### Environment variable reference

| Variable | Required | Description |
|---|---|---|
| `PROXMOX_VE_API_TOKEN` | Yes | Proxmox API token (read by provider directly) |
| `TF_VAR_proxmox_endpoint` | Yes | Proxmox VE API URL (e.g. `https://host:8006/`) |
| `TF_VAR_proxmox_ssh_username` | No | SSH user on Proxmox hosts (default: `terraform`) |
| `TF_VAR_ssh_private_key_path` | Yes | SSH private key for Proxmox host access |
| `TF_VAR_ssh_public_key_path` | Yes | SSH public key injected into VMs via cloud-init |
| `TF_VAR_nodes` | Yes | JSON array of Proxmox node objects |

Each node object in `TF_VAR_nodes`:

| Field | Example | Description |
|---|---|---|
| `name` | `pve1` | Proxmox node name (as shown in UI sidebar) |
| `address` | `pve1.lab.example.com` | SSH-reachable hostname or IP |
| `bridge` | `vmbr0` | Network bridge for VM interfaces |
| `datastore_id` | `local-lvm` | Storage pool for VM disks |

### Cluster variables (passed via `-var` or `TF_VAR_`)

| Variable | Default | Description |
|---|---|---|
| `cluster_name` | (required) | Cluster name — used in VM names and tags |
| `node_count` | `3` | Number of VMs to create (1-10) |
| `cpu_cores` | `4` | CPU cores per VM |
| `memory_mb` | `16384` | Memory per VM in MB |
| `disk_size_gb` | `32` | Disk size per VM in GB |
| `os_image` | `local:iso/debian-12-...` | Proxmox image for boot disk |

## Destroy

```sh
source .env
tofu destroy -var="cluster_name=dev1"
```

## SSH access

Connect to provisioned VMs using the cloud-init public key:

```sh
ssh -i ~/.ssh/your-cloud-init-key tf-user@<VM_IP>
```

## Known issues

- `~/spinifex/config/spinifex.toml` - Does not add node1, node2, node3 from config automatically
- `~/spinifex/config/predastore/predastore.toml` - Uses previous static local node config, needs to use the IPs for each node in the cluster
- On multi-node deployments, NATS on the primary can timeout waiting for other nodes to start, causing a race condition where NATS fails and all dependent services fail
- When adding 3 nodes, `nats.conf` is not updated with the 3rd node. Each node's cluster name is hardcoded to `C1` instead of per-node names
