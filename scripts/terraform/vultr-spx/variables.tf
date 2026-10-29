variable "deployment_name" {
  type        = string
  description = "Names every resource and the reserved-IP label prefix, so two deployments in one account never collide."
  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{0,23}$", var.deployment_name))
    error_message = "deployment_name must be 1-24 lowercase letters, digits or hyphens."
  }
}

variable "region" {
  type        = string
  default     = "ord"
  description = "Vultr region ID. Independent of the Spinifex region the cluster reports."
}

variable "plan" {
  type        = string
  default     = "vx1-g-16c-64g"
  description = "VX plan, 16 vCPU at least, or a vbm- bare-metal plan such as vbm-8c-64gb-amd."
}

variable "os_id" {
  type        = number
  default     = 2284
  description = "Vultr OS ID. 2284 is Ubuntu 24.04 LTS x64, the image Phase 1 was proved on."
}

variable "boot_disk_gb" {
  type        = number
  default     = 50
  description = "Size of each VM's bootable block volume. VX plans have no local root disk; bare metal ignores it."
}

variable "node_count" {
  type    = number
  default = 1
  validation {
    condition     = contains([1, 3], var.node_count)
    error_message = "node_count must be 1 or 3."
  }
}

variable "vpc_subnet" {
  type    = string
  default = "10.10.8.0"
}

variable "vpc_subnet_mask" {
  type    = number
  default = 24
}

variable "ssh_public_key_path" {
  type    = string
  default = "~/.ssh/id_ed25519.pub"
}
