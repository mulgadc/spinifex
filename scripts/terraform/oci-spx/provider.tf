provider "oci" {
  tenancy_ocid     = var.tenancy_ocid
  user_ocid        = var.user_ocid
  fingerprint      = var.fingerprint
  private_key_path = var.private_key_path
  region           = var.region
}

# The aliased home-region provider is gone with the identity resources it served.
# Everything here now lives in one region, in an existing compartment.
