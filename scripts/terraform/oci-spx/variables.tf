variable "tenancy_ocid" { type = string }
variable "user_ocid" { type = string }
variable "fingerprint" { type = string }
variable "private_key_path" { type = string }
# OCI's own region name. Spinifex's region is a separate setting that follows AWS
# naming, so ap-sydney-1 here beside ap-southeast-2 in spinifex.toml is correct.
variable "region" {
  type    = string
  default = "ap-sydney-1"
}
# Unused. Kept declared because scripts/oci_env.py discovers and exports it, and
# there are no identity resources left that need the home-region endpoint.
variable "home_region" {
  description = "Unused. The tenancy home region, exported by the Python environment tool."
  type        = string
  default     = null
  nullable    = true
}
# Required, and deliberately an OCID rather than a name. This deploys into a
# compartment that already exists; creating one needs tenancy-root rights that the
# API user Spinifex runs as must not have, and a name lookup can match a deleted one.
variable "compartment_ocid" {
  description = "OCID of the existing compartment to deploy into."
  type        = string
}

# Prefixes every resource this config owns, so nothing it creates can be confused
# with -- or collide with -- anything built by hand in the same compartment. The
# VCN dns_label takes this value directly, so keep it short and alphanumeric.
variable "deployment_name" {
  description = "Name prefix for every resource this configuration creates."
  type        = string
  default     = "spinifex"

  validation {
    condition     = can(regex("^[a-z][a-z0-9]{0,14}$", var.deployment_name))
    error_message = "deployment_name must be 1-15 lowercase alphanumeric characters starting with a letter, because it is used as the VCN dns_label."
  }
}
variable "vcn_cidr" {
  description = "VCN address space. A /22 leaves room for the per-node secondary private IPs Spinifex allocates for external addresses."
  type        = string
  default     = "10.200.0.0/22"
}
variable "public_subnet_cidr" {
  description = "Subnet every Spinifex node sits in, on both of its VNICs. Nodes serve the UI, predastore and the AWS gateway directly, so they cannot live in a private subnet."
  type        = string
  default     = "10.200.0.0/23"
}
variable "private_subnet_cidr" {
  description = "Unused by Spinifex today; guests live on the OVN overlay, not in an OCI subnet. Kept so the subnet exists if something later needs it."
  type        = string
  default     = "10.200.2.0/24"
}

variable "compute_shape" {
  description = "Compute shape for each Spinifex node. Bare metal is preferred; VM.Standard.E6.Flex is what we test on until the bare-metal quota is raised."
  type        = string
  default     = "VM.Standard.E6.Flex"
}

# A flex shape carries no size of its own, so these are not tuning knobs -- without
# them the shape is unbuildable. Both are the minimum recommended for a node.
variable "compute_ocpus" {
  description = "OCPUs per node on a flex shape. OCI counts an OCPU as a full core, so 8 OCPU is 16 vCPU. Ignored on a fixed bare-metal shape."
  type        = number
  default     = 8

  validation {
    condition     = var.compute_ocpus >= 8
    error_message = "compute_ocpus must be at least 8 (16 vCPU): the minimum recommended for a Spinifex node."
  }
}

variable "compute_memory_in_gbs" {
  description = "Memory per node in GiB on a flex shape. Ignored on a fixed bare-metal shape."
  type        = number
  default     = 32

  validation {
    condition     = var.compute_memory_in_gbs >= 32
    error_message = "compute_memory_in_gbs must be at least 32: the minimum recommended for a Spinifex node."
  }
}

variable "node_count" {
  description = "Number of Spinifex nodes, each with its own data volume. 1 for the single-node replica, 3 for a cluster."
  type        = number
  default     = 1

  validation {
    condition     = var.node_count >= 1 && var.node_count <= 25 && floor(var.node_count) == var.node_count
    error_message = "node_count must be a whole number between 1 and 25."
  }
}

variable "ubuntu_version" {
  description = "Canonical Ubuntu platform image version."
  type        = string
  default     = "26.04"
}

variable "data_volume_size_in_gbs" {
  description = "Size in GiB of the data block volume that backs /var/lib/spinifex on each node."
  type        = number
  default     = 256
}

variable "data_mountpoint" {
  description = "Where cloud-init mounts the data volume. Spinifex's data directory itself, so one mount rather than a mount plus a redirect, and no config points anywhere unusual."
  type        = string
  default     = "/var/lib/spinifex"
}

variable "data_volume_vpus_per_gb" {
  description = "Performance tier for each node's data volume in VPUs per GB."
  type        = number
  default     = 120

  validation {
    condition     = var.data_volume_vpus_per_gb >= 0 && var.data_volume_vpus_per_gb <= 120
    error_message = "data_volume_vpus_per_gb must be between 0 and 120."
  }
}

variable "ssh_public_key_path" {
  description = "Absolute path to the SSH public key installed on the Ubuntu instance."
  type        = string
  default     = null
  nullable    = true
}

# Nothing consumes this. It stays declared because scripts/oci_env.py exports it,
# and an undeclared TF_VAR_ is easier to misread as a broken helper than as unused.
variable "ssh_private_key_path" {
  description = "Unused. The vault that imported the private key was removed; no resource needs it."
  type        = string
  default     = null
  nullable    = true
}

variable "node_client_cidr_allow_list" {
  description = "CIDR blocks permitted to reach a node's SSH and Spinifex service ports from the internet."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

# The host firewall only. The OCI security list is deliberately open, because a
# guest's policy is its AWS security group and OVN enforces that on the node —
# see the comment on oci_core_security_list.public.
variable "node_service_ports" {
  description = "TCP ports the node itself serves: console, predastore gate, AWS gateway. SSH is always opened separately."
  type        = list(number)
  default     = [3000, 8443, 9999]
}

variable "wan_bridge_name" {
  description = "Linux bridge built over the secondary VNIC to carry the external datapath."
  type        = string
  default     = "br-wan"
}

variable "wan_bridge_mtu" {
  description = "MTU for the WAN bridge and its member interface. The OCI VCN carries 9000."
  type        = number
  default     = 9000
}
