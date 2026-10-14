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

# OCI serves IAM writes from the tenancy's home region, so the dynamic group and
# policy go through this one. Defaults to var.region, which makes it identical to
# the provider above until OCI_HOME_REGION says otherwise -- so a tenancy whose
# home region is where we build is unaffected, and ours is not.
provider "oci" {
  alias            = "home"
  tenancy_ocid     = var.tenancy_ocid
  user_ocid        = var.user_ocid
  fingerprint      = var.fingerprint
  private_key      = var.private_key != "" ? var.private_key : null
  private_key_path = var.private_key == "" ? var.private_key_path : null
  region           = var.home_region != null && var.home_region != "" ? var.home_region : var.region
}
