variable "deployment_name" {
  type        = string
  description = "Names every resource and the server hostnames, so two deployments in one account never collide."
  validation {
    # A hostname is at most 32 characters: "spx-" + name + "-node1".
    condition     = can(regex("^[a-z0-9][a-z0-9-]{0,15}$", var.deployment_name))
    error_message = "deployment_name must be 1-16 lowercase letters, digits or hyphens."
  }
}

variable "project" {
  type        = string
  default     = ""
  description = "ID or slug of an existing latitude.sh project to deploy into. Empty creates a project spinifex-<deployment_name> and removes it on destroy."
}

variable "project_id" {
  type        = string
  default     = ""
  description = "ID of the project named by project. The Elastic IP create sends it as project_id without resolving a slug (provider 4.8.0), so it must be an ID. Unused when project is empty."
}

variable "site" {
  type        = string
  default     = "DAL"
  description = "latitude.sh site slug. Pick one with stock for the plan and Elastic IP support; not SAO."
}

variable "plan" {
  type        = string
  default     = "m4-metal-medium"
  description = "Server plan slug (the pricing page calls it m4.metal.medium)."
}

variable "operating_system" {
  type        = string
  default     = "ubuntu_26_04_x64_lts"
  description = "OS slug. UNVERIFIED: the 26.04 slug follows the 24.04 pattern (ubuntu_24_04_x64_lts) and is checked by a data source at plan time."
}

variable "node_count" {
  type    = number
  default = 1
  validation {
    condition     = contains([1, 3], var.node_count)
    error_message = "node_count must be 1 or 3."
  }
}

variable "elastic_ip_count" {
  type        = number
  default     = 1
  description = "Elastic IPs to buy, each 4 USD a month. Every one starts on a server and keeps it unless Spinifex moves it. latitude.sh allows 5 per team."
  validation {
    condition     = var.elastic_ip_count >= 1 && var.elastic_ip_count <= 5 && floor(var.elastic_ip_count) == var.elastic_ip_count
    error_message = "elastic_ip_count must be a whole number from 1 to 5."
  }
}

variable "ssh_public_key_path" {
  type    = string
  default = "~/.ssh/id_ed25519.pub"
}
