# Spinifex Node Template Builder

Builds a Proxmox VM template with all Spinifex dependencies pre-installed. Cloning from this template reduces cluster spin-up from ~15 minutes to ~1-2 minutes (`git pull` + `make build`).

## What's in the template

| Component | Details |
|---|---|
| System packages | QEMU, nbdkit, libvirt, gcc, jq, curl, etc. (`make install-system`) |
| Go | 1.26.4 (`make install-go`) |
| AWS CLI | v2 (`make install-aws`) |
| Repositories | spinifex, viperblock, predastore (with `go.work` configured) |
| Module cache | `go mod download` completed for all repos |
| Cloud images | Ubuntu 26.04 for nested VMs (`~/images/ubuntu-26.04.img`) |
| Tuning | sysctl (rmem_max/wmem_max), kvm group, ufw disabled |
| Generalized | Clean machine-id, SSH host keys, cloud-init — each clone boots fresh |

## Quick Start

```bash
cd scripts/iac/proxmox-template

# 1. Configure
cp .env.example .env
# Edit .env — set your Proxmox endpoint, API token, node, and SSH key paths.

# 2. Set API token (not stored in .env for security)
export PROXMOX_VE_API_TOKEN="terraform@pve!provider=YOUR_TOKEN"

# 3. Build
source .env
tofu init
tofu plan
tofu apply
```

Build takes ~10-15 minutes (apt packages, Go, cloud image download). When done, you'll have a template on the build node at the configured VMID (default 9000).

## Distribute to Other Nodes

After `tofu apply`, the output prints ready-to-run commands. Example for a 3-node cluster:

```bash
# Export from build node
ssh -i ~/.ssh/proxmox-host-tf terraform@<BUILD_NODE> \
  'vzdump 9000 --dumpdir /tmp --mode stop --compress zstd'

# Copy to other nodes
scp -3 -i ~/.ssh/proxmox-host-tf \
  terraform@<BUILD_NODE>:/tmp/vzdump-qemu-9000-*.vma.zst \
  terraform@<TARGET_NODE>:/tmp/

# Restore on each target node (use a unique VMID and the node's datastore)
ssh -i ~/.ssh/proxmox-host-tf terraform@<TARGET_NODE> \
  'qmrestore /tmp/vzdump-qemu-9000-*.vma.zst <NEW_VMID> --storage <DATASTORE> && qm template <NEW_VMID>'
```

After distributing, each node has its own copy of the template with a unique VMID.

## Using the Template

Update `scripts/iac/proxmox/vms.tf` to clone from the template instead of booting from a raw cloud image. Or clone manually from the Proxmox UI.

On each cloned VM after boot:

```bash
# Pull latest code
cd ~/Development/mulga/spinifex && git pull
cd ~/Development/mulga/viperblock && git pull
cd ~/Development/mulga/predastore && git pull

# Build (~30s with cached modules)
cd ~/Development/mulga/spinifex && export PATH=/usr/local/go/bin:$PATH && make build
```

## Testing a Different Base Image

Upload the new image to Proxmox, then build a separate template:

```bash
# Debian 13 example
export TF_VAR_base_image="local:iso/debian-13-genericcloud-amd64.img"
export TF_VAR_base_image_tag="debian-13"
export TF_VAR_template_vmid=9010   # Keep the debian-12 template at 9000

source .env
tofu apply
```

Templates are tagged in the Proxmox UI with the base image name (`debian-12`, `debian-13`, etc.) so you can tell them apart.

## Rebuilding

When dependencies change (new Go version, new system packages):

```bash
source .env
tofu destroy     # Remove the old template
tofu apply       # Build fresh
# Then re-distribute with vzdump
```

## SSH Key Setup

This template uses **two separate key pairs** (matching the `scripts/iac/proxmox/` setup):

| Key | Used for | Variable |
|---|---|---|
| Proxmox host key | SSH to Proxmox hosts (provider, `qm template` command) | `ssh_private_key_path` |
| VM / cloud-init key | SSH to VMs (cloud-init injection, provisioning) | `vm_ssh_private_key_path` + `ssh_public_key_path` |

If you use the same key for both, set all three variables to the same key pair.

## Files

| File | Description |
|------|-------------|
| `main.tf` | Proxmox provider configuration |
| `variables.tf` | All configurable variables with defaults |
| `template-builder.tf` | VM creation, provisioning, template conversion |
| `outputs.tf` | Template info + vzdump/restore commands |
| `scripts/provision.sh` | Install deps, clone repos, cache modules, generalize |
| `.env.example` | Environment variable template |
| `.gitignore` | Ignores .env, state files, provider plugins |
