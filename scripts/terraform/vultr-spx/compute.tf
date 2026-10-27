resource "vultr_ssh_key" "deploy" {
  name    = "spinifex-${var.deployment_name}"
  ssh_key = trimspace(file(pathexpand(var.ssh_public_key_path)))
}

# The boot volume is labelled so destroy can find it: Vultr keeps a VX boot
# volume, and bills it, after the instance that booted from it is deleted.
resource "vultr_instance" "node" {
  count       = var.node_count
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
