# A runner gets the key as a value, a workstation as a path. Passing the value
# means CI never writes key material to disk, so there is nothing to leave behind
# on a shared runner and nothing to clean up after a cancelled job.
provider "oci" {
  tenancy_ocid     = var.tenancy_ocid
  user_ocid        = var.user_ocid
  fingerprint      = var.fingerprint
  private_key      = var.private_key != "" ? var.private_key : null
  private_key_path = var.private_key == "" ? var.private_key_path : null
  region           = var.region
}

# The aliased home-region provider is gone with the identity resources it served.
# Everything here now lives in one region, in an existing compartment.
