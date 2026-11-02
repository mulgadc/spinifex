resource "vultr_ssh_key" "deploy" {
  name    = "spinifex-${var.deployment_name}"
  ssh_key = trimspace(file(pathexpand(var.ssh_public_key_path)))
}

# A vbm- plan is a bare-metal server; anything else is a VX VM.
locals {
  bare_metal = startswith(var.plan, "vbm-")
}

# The boot volume is labelled so destroy can find it: Vultr keeps a VX boot
# volume, and bills it, after the instance that booted from it is deleted.
resource "vultr_instance" "node" {
  count       = local.bare_metal ? 0 : var.node_count
  region      = var.region
  plan        = var.plan
  os_id       = var.os_id
  label       = "spinifex-${var.deployment_name}-node${count.index + 1}"
  hostname    = "node${count.index + 1}"
  ssh_key_ids = [vultr_ssh_key.deploy.id]
  vpc_ids     = [vultr_vpc.lan.id]
  user_data   = file("${path.module}/cloud-init/user-data.yaml")
  tags        = ["spinifex", "deployment:${var.deployment_name}"]
  backups     = "disabled"

  block_devices {
    disk_size = var.boot_disk_gb
    bootable  = true
    label     = "spinifex-${var.deployment_name}-node${count.index + 1}-boot"
  }
}

# Bare metal boots from its local NVMe, so there is no boot volume to label.
resource "vultr_bare_metal_server" "node" {
  count            = local.bare_metal ? var.node_count : 0
  region           = var.region
  plan             = var.plan
  os_id            = var.os_id
  label            = "spinifex-${var.deployment_name}-node${count.index + 1}"
  hostname         = "node${count.index + 1}"
  ssh_key_ids      = [vultr_ssh_key.deploy.id]
  vpc_id           = vultr_vpc.lan.id
  user_data        = file("${path.module}/cloud-init/user-data.yaml")
  tags             = ["spinifex", "deployment:${var.deployment_name}"]
  activation_email = false

  timeouts {
    create = "60m"
  }
}
